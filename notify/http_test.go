package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// This file covers doJSON's HTTP-layer error classification.
//
// Why it is tested separately: each channel adapter is only
// responsible for the platform's own business error codes (DingTalk
// errcode, Feishu code, Telegram ok field), while the **HTTP layer**
// classification is done uniformly by doJSON — the two are
// independent lines of defense. Without this one, a 503-returning
// transit gateway would be judged a permanent failure and give up on
// retry; while a 403 would be judged retryable and just whitewash
// through three rounds of backoff for nothing.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 success is not an error", 200, false},
		{"429 rate-limited retryable", 429, false},
		{"408 request timeout retryable", 408, false},
		{"500 server error retryable", 500, false},
		{"502 gateway error retryable", 502, false},
		{"503 service unavailable retryable", 503, false},
		{"400 bad parameter permanent", 400, true},
		{"401 auth failure permanent", 401, true},
		{"403 forbidden permanent", 403, true},
		{"404 not found permanent", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx should not error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("non-2xx should error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d permanent judgment wrong: expected %v, got %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// The status code must appear in the error,
			// otherwise the user has no way to tell whether they
			// misconfigured or the peer is down.
			// We assert on the number rather than Go's English
			// StatusText: the wording in this package is English
			// (consistent with the rest of the project), and the
			// number is the language-neutral, stably-assertable
			// part.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("error message should include the HTTP status code %d, got %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet covers snippet: the peer's
// error explanation must be brought back, otherwise the user only
// knows "it failed" and not why the peer rejected.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should error")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("error message should bring back the peer's explanation, got %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded constrains the shape of
// snippet: the peer's response goes verbatim into the last_error
// column and the front-end table, and multi-line / overlong content
// would break layout and payload.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// A response with newlines, tabs, and 5000 characters of
	// overlong content.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should error")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("error message should be compressed into a single line, got %q", msg)
	}
	// snippet upper limit 200 chars + fixed prefix, total must be
	// far smaller than the original response.
	if len(msg) > 400 {
		t.Errorf("error message too long (%d bytes), should be truncated by snippet: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse confirms that reads have a
// limit: when the peer abnormally returns huge content, the entire
// response must not be read into memory (every entry in the
// delivery history stores a copy of last_error).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should error")
	}
	if len(err.Error()) > 400 {
		t.Errorf("an overlong response should be read with a limit and truncated, error message length %d", len(err.Error()))
	}
}
