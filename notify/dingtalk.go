package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel implements the DingTalk custom bot.
//
// Platform characteristics (which drive the implementation choices here):
//   - A single bot is rate-limited to 20 messages/minute; anything over that is
//     silently dropped (the HTTP response may still be 200), so rate limiting
//     must happen client-side — see DefaultRatePerMin.
//   - Pick one of three security settings: signing / custom keywords / IP allowlist.
//     Signing is the only option that does not depend on the message content, so
//     only signing is supported (a bare webhook with none of the three enabled
//     also works).
//   - Both success and failure return HTTP 200; they are distinguished by the
//     errcode in the body — not checking errcode records a failed delivery as a
//     success.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk's webhook URL carries an access_token and is itself a credential, so
// the whole thing is masked.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The destination is DingTalk's webhook URL itself; changing the URL requires
// re-confirming the signing key for the new URL at the same time.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid webhook URL: %w", err)
	}
	return nil
}

// Send delivers one message. When there is a callback URL and it is a single
// item, an ActionCard (with a button) is used; otherwise markdown is used.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk markdown bodies have no explicit byte limit, but we still cap it
	// to guard against an evidence field blowing up abnormally.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "View details",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk hides business errors inside a 200 response.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 means signature verification failed, 310000 means a keyword
		// mismatch — both are configuration errors that a retry will not heal.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends the timestamp and sign parameters to the webhook
// according to the official signing rules.
//
// Rule: the string to sign = timestamp + "\n" + secret, and the HMAC-SHA256
// **key is also the secret**; the result is base64-encoded and then URL-encoded.
// The timestamp is in milliseconds. When secret is empty the hook is returned
// unchanged, to support bots that have not enabled signing.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// Do not pass err through: url.Parse's error text carries the full URL
		// (including the access_token).
		return "", fmt.Errorf("failed to parse webhook URL: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL checks that the URL is usable and its scheme is supported, and
// performs an internal-network check on literal IP targets.
//
// Two points of care:
//
//  1. **Error messages must be redacted.** url.Parse itself returns a *url.Error
//     whose Error() carries the **full original URL**, and the URLs for these
//     providers embed credentials (DingTalk access_token, WeCom key, Telegram
//     bot token, Feishu hook id). This once did a plain `return err`, so the
//     "invalid URL format" error leaked the credential, flowing into the test
//     endpoint's 400 response, the last_error stored on every delivery, the
//     server logs, and the delivery-history endpoint.
//
//  2. **A literal IP is judged as internal right away**, while hostnames are
//     left to the dial stage (blockInternalDial is the final point of
//     enforcement and also covers DNS rebinding). Doing it once here is so that
//     a hint can be given at config-save time rather than waiting for the first
//     delivery to fail.
//
// Restricting the scheme is defensive: things like file:/// or gopher:// can make
// http.Client behave unexpectedly (already blocked by the scheme check, but
// there is no reason to open up that surface).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("address could not be parsed (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https is supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing hostname")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("refusing to deliver to loopback/link-local address %s (to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
