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
//   ① the delivery address must not use the server as a pivot to reach the internal network / cloud metadata (SSRF)
//   ② the address-validation error message must not carry out the credentials in the address
//
// On the test environment: many cases in this package use httptest fake receivers on 127.0.0.1, which the guard blocks by default.
// So TestMain turns on AllowLocalTargetsEnv globally, while each SSRF case below
// explicitly clears it to assert the **deny-by-default** behavior.

func TestMain(m *testing.M) {
	// Let regular cases connect to the local fake receiver; SSRF cases clear it temporarily themselves.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core assertion of the SSRF protection:
// in the default configuration, delivery to a loopback address must be refused at the **connection layer**.
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
		t.Fatal("delivery to a loopback address should not be allowed by default")
	}
	if hit {
		t.Fatal("the request reached the local service — the guard did not take effect")
	}
	// The error message must guide the user on how to allow it (a local SMTP relay is a legitimate config).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the rejection message should explain how to explicitly allow it: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the reverse case: once explicitly enabled it must work,
// or legitimate deployments like a local postfix / internal relay get wiped out across the board.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("delivery should work after explicitly allowing it: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // cloud metadata endpoint — the main reason this function exists
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped form must be unwrapped before judging, otherwise it is a bypass
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be rejected", s)
		}
	}
	// RFC1918 private networks are **deliberately allowed**: a self-hosted Mattermost / SMTP relay on an internal network is a common legitimate use.
	// This assertion pins down that tradeoff — if someone later casually adds a private-network check, this fails,
	// forcing a conscious decision (rather than silently wiping out a batch of deployments).
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (private networks are a common legitimate delivery target)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets covers the pre-check at configuration time:
// a literal IP should be rejected at save time, not only after the first delivery fails.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected at configuration time", raw)
		}
	}
	// Public and private-network addresses pass as usual (private networks are left to the dial stage, which does not block them).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials is a branch the audit pointed out I missed last round.
//
// When url.Parse **fails** it returns a *url.Error whose Error() contains the full original address. Last round I only
// redacted the error returned by http.Client.Do and missed this; and the "permanent-failure path" cases I added then
// (file://, gopher://, ftp://) all actually parse successfully with url.Parse and take the scheme branch,
// so all-green did not prove this path safe — a false guarantee.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // invalid percent escape
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // non-numeric port
		"http://[::1?access_token=" + leakProbeToken,                  // unbalanced brackets
	}
	for _, raw := range cases {
		// First confirm this input **really** makes url.Parse fail. Without this step the case could silently
		// take another branch (that is exactly how last round's false guarantee arose).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q should have failed to parse, otherwise this case does not cover the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Confirm the channel-layer wrapper also does not carry the address out.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("an invalid address should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault covers the dial guard of the SMTP channel.
//
// The email channel once used a bare net.Dialer, the only gap in the whole SSRF protection: setting host to
// 169.254.169.254 or 127.0.0.1 could connect directly, and when smtp.NewClient's handshake failed it would wrap
// the peer's returned line into the error, echoed via last_error by the delivery-history API — exactly the
// half-blind read primitive the other channels already closed; the "connection refused vs timeout" timing difference can also probe ports.
//
// This package's TestMain turns on AllowLocalTargetsEnv globally (many cases use a fake receiver on 127.0.0.1),
// so this case must clear it itself — otherwise it would pass whether the guard exists or not,
// which is exactly why the gap went undetected by any test originally.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // turn off the escape hatch = default behavior
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("email delivery to a loopback address should not be allowed by default")
	}
	// The connection should never be established: the guard blocks it in the Control hook, so EHLO is never sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("the SMTP session was already established — the guard did not take effect")
	}
	// The error message must guide the user on how to allow it (a local postfix relay is a legitimate config).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the rejection message should explain how to explicitly allow it: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired reverse case: once explicitly enabled
// it must deliver normally. A self-hosted internal SMTP / local relay is a very common deployment, and the guard must not be a blanket block.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("local SMTP should deliver after explicitly allowing it: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("no EHLO seen — the session was not actually established")
	}
}
