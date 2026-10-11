package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard upper bound (in bytes, not characters)
// for WeCom group bot markdown content. This is the tightest limit of
// all six channels, and the main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements the WeCom group bot.
//
// Platform characteristics:
//   - The only authentication is the key on the URL; signing is not
//     supported — so the webhook URL itself is the entire credential.
//   - The markdown content limit is 4096 **bytes**; overlong messages
//     are rejected (not truncated). With 3 bytes per Chinese
//     character, the body has only a little over a thousand
//     characters to write, and the client must truncate.
//   - Rate limit 20/minute, similarly caught by client-side rate
//     limiting.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has only one credential (the key on the URL), and it does not
// support signing — the whole URL is the entire credential, no other
// field needs masking.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has only the Webhook field; it is both destination and
// credential, so there is no "credentials left behind after the
// address is changed" to worry about.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook URL: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// A batch summary can be very long (50 items × one line each +
	// prefix), and 4096 bytes is easy to exceed.
	// Truncate here rather than relying on the platform's error:
	// rejection means the whole batch is lost, while truncation at
	// least delivers the first few items.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse WeCom response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 is "interface call exceeded the limit" — the
		// platform's rate-limit window rolls, so backoff retry is
		// effective, hence explicitly classified as retryable.
		// Reaching here means rate_per_min is set too aggressively;
		// retry is just a fallback, the real fix is to lower the
		// channel's rate limit.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate limited %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 is an invalid webhook key — permanent failure,
		// retry won't self-heal.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
