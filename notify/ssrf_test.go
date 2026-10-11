package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// This file covers two related hardening measures:
//   ① The delivery address must not turn the server into a stepping
//      stone to hit internal networks or cloud metadata (SSRF)
//   ② The error message from address validation must not leak
//      credentials inside the address
//
// Regarding the test environment: this package uses httptest stub
// receivers on 127.0.0.1 extensively, which the guard would block by
// default. So TestMain turns on AllowLocalTargetsEnv globally, while
// each of the SSRF test cases below explicitly clears it to assert the
// **default-deny** behavior.

func TestMain(m *testing.M) {
	// Let normal tests connect to local stub receivers; SSRF tests
	// clear it themselves on demand.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core assertion of the
// SSRF protection: under default configuration, delivery to a loopback
// address must be rejected at the **connection layer**.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // turn off the escape hatch = default behavior
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("delivery to loopback address should not be allowed by default")
	}
	if hit {
		t.Fatal("request already reached the local service — the guard is not effective")
	}
	// The error message must tell the user how to opt in (local SMTP
	// relay is a legitimate configuration).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("rejection message should explain how to explicitly opt in: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the reverse case: it must
// work after explicitly opting in, otherwise legitimate deployments
// such as a local postfix or internal relay would be wiped out by a
// blanket block.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("after explicit opt-in, delivery should succeed: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // cloud metadata endpoint — the main reason this function exists
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped form must be reduced before judging, otherwise this is a bypass
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be rejected", s)
		}
	}
	// RFC1918 private networks are **deliberately allowed**: self-hosted
	// Mattermost / SMTP relays on the internal network are a common
	// legitimate use case. This assertion pins that trade-off — if
	// someone later casually adds a private-network check, this test
	// will fail, forcing a deliberate decision (rather than silently
	// breaking a batch of deployments).
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (private networks are common legitimate delivery targets)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets covers the upfront
// hint at the config stage: literal IPs should be rejected at save
// time, rather than waiting for the first delivery to fail.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected at the config stage", raw)
		}
	}
	// Public and private addresses pass as usual (private is left for
	// the dial stage, where it is not blocked).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials covers a branch the
// audit pointed out as missed in my previous round.
//
// When url.Parse **fails**, it returns *url.Error, whose Error() contains
// the full original address. The previous round only redacted the error
// returned by http.Client.Do, missing this; the "permanent failure path"
// cases added at the time (file://, gopher://, ftp://) all parse
// successfully and go through the scheme branch, so a green test suite
// did not actually prove this path was safe — a false guarantee.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // illegal percent-escape
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // port is not a number
		"http://[::1?access_token=" + leakProbeToken,                  // mismatched brackets
	}
	for _, raw := range cases {
		// First confirm this input does indeed make url.Parse fail.
		// Without this step, the test could silently walk into a
		// different branch (which is exactly how the previous round's
		// false guarantee happened).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q should have failed to parse, otherwise this test is not covering the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Confirm that the channel-level wrapping also does not leak the
	// address.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("invalid address should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault covers the dial guard for
// the SMTP channel.
//
// The email channel used to use a bare net.Dialer, which was the only
// gap in the entire SSRF protection: filling in 169.254.169.254 or
// 127.0.0.1 as host could connect directly, and when smtp.NewClient's
// handshake failed it wrapped the peer's returned line into the error,
// echoed back through last_error by the delivery history endpoint —
// exactly the half-blind read primitive that other channels had
// already shut down; the time difference between "connection refused"
// and "timeout" could also be used to probe for open ports.
//
// This package's TestMain turns on AllowLocalTargetsEnv globally
// (many tests use stub receivers on 127.0.0.1), so this test must
// clear it itself — otherwise the test passes whether the guard is in
// place or not, which is exactly why no test caught the gap at first.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // turn off the escape hatch = default behavior
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("delivery of email to loopback address should not be allowed by default")
	}
	// The connection should never have been established: the guard
	// blocks it in the Control hook, so EHLO is never sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP session has been established — the guard is not effective")
	}
	// The error message must tell the user how to opt in (local
	// postfix relay is a legitimate configuration).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("rejection message should explain how to explicitly opt in: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired reverse
// case: after explicit opt-in, delivery must work normally. Internal
// self-hosted SMTP / local relays are a very common deployment, so the
// guard cannot be a blanket block.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("after explicit opt-in, local SMTP should be deliverable: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO was not seen — the session was not actually established")
	}
}