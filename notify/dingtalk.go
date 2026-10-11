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
// Platform characteristics (which drive the implementation trade-offs
// here):
//   - Single bot rate limit of 20 messages/minute, oversends are
//     silently dropped (HTTP can still be 200), so rate limiting must
//     happen on the client side, see DefaultRatePerMin.
//   - One of three security settings: signing / custom keyword / IP
//     whitelist. Signing is the only one that does not depend on
//     message content, so only signing is supported (and bare webhooks
//     with none of the three are also supported).
//   - Both success and failure return HTTP 200; the body carries an
//     errcode to distinguish them — not checking errcode would log a
//     failed delivery as successful.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk's Webhook URL carries the access_token, which is itself
// the credential, so it is masked as a whole.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The destination is the Webhook URL itself; changing the address
// must come with restating the signing secret for the new address.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook URL: %w", err)
	}
	return nil
}

// Send delivers one message. When there is a link back and it's a
// single item, use ActionCard (with a button); otherwise use markdown.
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
	// DingTalk's markdown body has no explicit byte limit, but we
	// still apply an upper bound to avoid evidence fields ballooning
	// abnormally.
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
	// DingTalk hides business errors inside the 200 response.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 is signature verification failure, 310000 is keyword
		// mismatch — both are config errors, retry won't self-heal.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends the timestamp and sign parameters to the
// webhook per the official signing rules.
//
// Rule: the string to sign is timestamp + "\n" + secret, the HMAC-SHA256
// **key is also secret**, and the result is base64-encoded then
// URL-encoded. The timestamp is in milliseconds. When secret is empty
// the URL is returned unchanged, to support bots without signing.
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
		// Do not pass err through: url.Parse's error text contains
		// the full URL (including access_token).
		return "", fmt.Errorf("failed to parse Webhook URL: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL validates that the address is usable, that the
// protocol is supported, and applies an internal-network check for
// literal-IP targets.
//
// Two things to be careful about:
//
//  1. **The error message must be redacted**. What url.Parse itself
//     returns is *url.Error, whose Error() carries the **full original
//     address**, and these platforms' addresses embed credentials
//     inside (DingTalk access_token, WeCom key, Telegram bot token,
//     Feishu hook id). This used to just `return err`, so the
//     "invalid address format" error carried the credential out, into
//     the 400 response of the test endpoint, last_error recorded for
//     each delivery, server logs, and the delivery history endpoint.
//
//  2. **Literal IPs are checked for internal addresses directly**;
//     domain names are left to the dial stage (blockInternalDial is
//     where the final effect happens, and it also covers DNS
//     rebinding). Doing it here lets the user get a hint when saving
//     the config, rather than waiting for the first delivery to fail.
//
// Restricting the protocol is defensive: file:///gopher:// etc. would
// cause http.Client to behave unexpectedly (already blocked by the
// scheme check, but there is no reason to leave that surface open).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("address cannot be parsed (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https are supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing hostname")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("refusing to deliver to local/link-local address %s (if delivery to a local service is really required, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
