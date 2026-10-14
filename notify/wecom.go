package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard cap on a WeCom group bot's markdown content (in
// bytes, not characters). It is the tightest limit among all six channels and the
// main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements the WeCom (WeChat Work) group bot.
//
// Platform characteristics:
//   - It is the only one that authenticates via a key in the URL and does not
//     support signing — so the webhook URL itself is the entire credential.
//   - markdown content is capped at 4096 **bytes**, and anything over the limit
//     rejects the whole message (not truncates it). Chinese characters take 3
//     bytes each, meaning the body only has room for a bit over a thousand
//     characters, so client-side truncation is mandatory.
//   - Rate-limited to 20 messages/minute, likewise backstopped by client-side
//     rate limiting.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has only one credential — the webhook (the key in the URL) — and it does
// not support signing: the whole URL is the entire credential, and there are no
// other fields to mask.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has only the webhook field, which is both the destination and the
// credential, so there is no such thing as "a credential left behind after
// changing the URL".
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid webhook URL: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// A digest batch can be very long (50 items × one line each + a prefix), and
	// 4096 bytes is easily exceeded. Truncation is done here rather than relying
	// on a platform error: being rejected means the whole batch is lost, whereas
	// truncation at least delivers the first several items.
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
		// 45009 means the API call exceeded the limit — the platform's rate-limit
		// window rolls, so retrying after a backoff is effective, hence it is
		// explicitly classified as retryable. Reaching here means the client-side
		// rate_per_min is configured too aggressively; the retry is only a
		// backstop, and the real fix is to lower this channel's rate limit.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate-limited %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 means an invalid webhook key — a permanent failure that a retry
		// will not heal.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
