package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets decides whether messages may be delivered to loopback
// or link-local addresses.
//
// The default is to refuse. These address ranges are not where IM bots or
// public mail servers live, and what they can hit is very sensitive: an
// admin port of another service on the same host, and the cloud
// environment's metadata endpoint (169.254.169.254, which can read out
// instance credentials). The delivery address is configured by an admin,
// but a management session borrowed by XSS/CSRF, or a second person
// sharing the same JWT, can read the response back just by changing the
// config — doJSON writes the first 200 bytes of 4xx/5xx response bodies
// into last_error, and the delivery history endpoint echoes it back;
// that's a half-blind read primitive.
//
// But "local SMTP relay" (postfix on 127.0.0.1:25) is a common
// self-hosted mail configuration, and a blanket block would lock people
// out. So we leave an explicit escape hatch instead of hardcoding an
// allow: set ARTEX_NOTIFY_ALLOW_LOCAL=1 to permit it.
//
// Exported as AllowLocalTargetsEnv so tests can turn it on explicitly —
// this package and the server package's tests use httptest stub
// receivers on 127.0.0.1 extensively; without enabling it they would all
// be blocked by the guard.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP belongs to an
// "delivery disallowed by default" address range.
//
// We only refuse loopback, link-local (including the cloud metadata
// 169.254.169.254), unspecified, and multicast. We **do not** refuse
// RFC1918 private networks: self-hosted Mattermost / SMTP relays on the
// internal network are a perfectly legitimate use case, and blocking
// them outright would make the feature unusable in real environments.
// This trade-off is deliberate — the protection must block what is
// truly sensitive, while not breaking normal deployments.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6 (::ffff:127.0.0.1) must be reduced to IPv4 before
	// checking, otherwise the check is bypassed.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is the http.Transport dialer's Control hook, which
// checks the target address at the time the **connection is being
// established**.
//
// Why at the dial stage instead of only validating when the config is
// saved: this is where the final effect is. It also covers two ways to
// bypass config validation — DNS rebinding (resolved to a public IP at
// validation time, but to an internal IP at real connect time) and
// redirection (although we already refuse cross-host jumps, same-host
// jumps can still reroute the path to somewhere else).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("cannot parse target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("refusing to deliver to local/link-local address %s (if delivery to a local service is really required, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport only adds a dialer guard on top of the default
// Transport. It uses Clone to keep all the default's tuning (connection
// pool, HTTP/2, timeouts, proxy, etc.), so adding one check does not
// change any other behavior.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the shared client used by all channel deliveries.
//
// It deliberately does **not** reuse the project's global egress proxy
// (the server side's GlobalProxy): that proxy is for penetration target
// traffic and is often an unstable tunnel, and notification availability
// should not be held hostage to target network flakiness. IM push can
// connect directly. Timeout set to 15 seconds — anything slower is
// effectively a fault state.
//
// Refuses cross-host redirects: this feature's delivery addresses are
// "a fixed endpoint" shape and would not normally redirect to a
// different host; the credentials of these platforms (DingTalk's
// access_token, WeCom's key, Telegram's bot token) are **right in the
// URL**, so following a cross-host jump is equivalent to handing the
// credentials to the redirect target. Same-host jumps (such as
// appending a trailing slash) are still allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing cross-host redirect (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit limits the size of response bodies read. The peer may
// return very large content on error, while we only need the error
// code and a short error description for display in the delivery
// history.
const respBodyLimit = 8 << 10

// doJSON sends a request and returns the (size-limited) response body.
//
// When payload is nil, an empty body is sent (used for GET or scenarios
// where the platform does not require a body). Keys/values in headers
// are attached as-is, for use by the generic Webhook's custom headers.
//
// Error classification is the core responsibility of this function:
// network-level failures and 5xx/408/429 are classified as "retryable";
// the remaining 4xx are "permanent failure" — retrying a 403 just
// writes the same error into the log three times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// Serialization failure is a local bug (wrong field type in
			// the config), and retrying won't help.
			return nil, Permanent(fmt.Errorf("failed to build request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// Invalid URL — usually the user entered the address wrong,
		// which is a permanent failure.
		// We also cannot pass err through here: url.Parse's error text
		// contains the full address.
		return nil, Permanent(fmt.Errorf("invalid request URL: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout — mostly transient
		// faults, handed to backoff retry.
		//
		// Error text must be redacted before being passed out. Reason:
		// http.Client.Do returns *url.Error, whose Error() is
		// `Op "full URL": underlying error`, and the credentials of
		// these platforms are **right in the URL** (DingTalk
		// access_token, WeCom key, Feishu hook id, Telegram
		// /bot<token>/). Without redaction, credentials leak through
		// this error string to four places: notification_deliveries'
		// last_error (stored in plaintext), the delivery history
		// endpoint's response (**bypassing the channel config's
		// masking**), server logs, and the 502 text returned to the
		// frontend by the test-send endpoint.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limited) and 408 (timeout) are worth retrying; the
	// other 4xx are config or permission issues, retrying is
	// pointless.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("peer rate-limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("peer service error (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("peer rejected request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet compresses the response body into a single short line, used
// for error messages. Responses may contain newlines and lots of
// whitespace; directly inserting them into last_error would break the
// delivery history page's layout.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget compresses the delivery address to
// "scheme://host/…", used for error messages.
//
// This is the package's only URL redaction convention, deliberately
// made **brutal enough**: aside from the scheme and host, everything
// else is discarded. The reason is that there is no "universal and
// safe" way to tell which segment of a URL is the credential:
//
//	DingTalk  credential in query        /robot/send?access_token=xxx
//	WeCom     credential in query        /cgi-bin/webhook/send?key=xxx
//	Feishu    credential in **path tail** /open-apis/bot/v2/hook/<hook_id>
//	Telegram  credential in **path mid**  /bot<token>/sendMessage
//
// Trying to "keep only the useful part" would require patching per
// channel, and missing any single platform is a credential leak.
// Keeping the host is already enough for troubleshooting (DNS
// resolution failure, connection failure, certificate issues can all be
// located), and the specific bot is recognized by the masked tail hint
// in the channel config.
//
// Returns a fixed placeholder on parse failure — never echo back the
// raw string.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(address not parseable)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError strips the address from a transport-layer error,
// keeping only the underlying cause.
//
// *url.Error's structure is {Op, URL, Err}, and Error() prints the URL
// along with it. Here we explicitly take the Err field, bypassing its
// Error() — more reliable than doing string substitution afterwards,
// because substitution must correctly handle the various forms after
// URL encoding/escaping, which is easy to miss.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// Non-*url.Error (such as errors returned by the redirect policy)
	// may also carry the address; uniformly go through redaction.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces http(s) addresses appearing in a piece of
// text with their redacted form.
//
// Used as a fallback for errors where structured fields cannot be
// obtained (redirect policy errors, custom errors from third-party
// libraries). It only recognizes http/https prefixes and splits on
// whitespace and quotes — addresses will not contain those characters.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
