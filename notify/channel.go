package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is an adapter for a notification channel. Implementations
// must be **stateless**: the same instance is reused concurrently
// across multiple channel configurations, and credentials are always
// passed in via the cfg parameter.
type Channel interface {
	// Kind returns the channel type identifier; it must match the
	// registry's key.
	Kind() string
	// Validate is called when saving the configuration; it checks
	// required fields and format. The returned error is shown
	// directly to the configurer, so the wording should say "which
	// field is missing" rather than a vague "config invalid".
	Validate(cfg map[string]any) error
	// Send delivers one message, returning the **actual number of
	// items delivered** and an error.
	//
	// Why the count is returned: each platform has a message length
	// limit, and summary messages that don't fit the whole batch are
	// truncated. If the caller unconditionally marks the whole batch
	// as delivered, the truncated items are lost — not visible in
	// the message, the delivery history shows success, and there is
	// no place to discover that the finding was never sent. After
	// returning kept, the caller only marks the first kept items,
	// and leaves the rest for the next batch.
	//
	// Returning an error means delivery failed; *PermanentError means
	// it should not be retried. On failure kept is meaningless and
	// the caller should ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the platform-recommended per-minute
	// delivery upper bound, used as the default rate limit when
	// creating a new channel instance. Returning 0 means no known
	// limit.
	DefaultRatePerMin() int
	// SecretKeys returns the names of keys in this channel's config
	// that count as credentials. When the API echoes back, the
	// values of these keys are masked; when updating, receiving a
	// masked value means "keep the original stored value". Only the
	// implementation itself knows which fields are credentials (the
	// entire WeCom Webhook URL is a credential, while for DingTalk
	// only the secret is), so this knowledge must be supplied by the
	// channel, not guessed at by upper layers.
	SecretKeys() []string
	// DestinationKeys returns the names of keys in this channel's
	// config that decide "where the message is sent".
	//
	// Like SecretKeys this is a security-related concept: the
	// destination address and the credentials are two independent
	// sets of fields, and if "only change the address, keep the
	// credentials as-is" is allowed, anyone who can change the
	// channel config can send the real stored credentials to a
	// server they control, and the channel config's mask loses all
	// meaning.
	// See PrepareConfigUpdate.
	DestinationKeys() []string
}

// registry is the channel registry. It deliberately uses an explicit
// literal rather than init() self-registration: that way "which
// channels exist" is visible in one place, and adding a new channel
// surfaces omissions at compile time rather than via runtime side
// effects.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get retrieves a channel implementation by type.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns all supported channel types, sorted lexicographically
// (for a stable UI dropdown display).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that should not be retried:
// bad credentials, target rejection, invalid request body, etc.
// Retry is only meaningful for transient faults (network flakiness,
// rate limiting, peer 5xx); repeatedly backing off and retrying a
// permanent failure will not succeed, and will also wash the real
// error out of the retry log.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. Returns nil when err
// is nil, so it can be written as `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether the err chain carries a permanent
// failure marker.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- config reading helpers ----
//
// Channel config comes from a JSONB column in the database, which
// after encoding/json deserialization is map[string]any, with
// numbers as float64 and arrays as []any. The helpers below
// normalize this layer of conversion, and tolerate type drift
// caused by users leaving fields blank in the UI (e.g. typing the
// port as a string).

// cfgString reads a string config entry, trimming leading and
// trailing whitespace — easy to pick up from copy-pasting in a web
// form.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt reads an integer config entry, compatible with both
// float64 (the JSON default) and string sources.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool reads a boolean config entry, compatible with the strings
// "true"/"1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings reads a string array config entry, automatically
// trimming whitespace and dropping empty strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// Also accepts a single string, for form submission when
		// there is only one value.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap reads a string-map config entry (e.g. custom HTTP
// headers), trims whitespace from both keys and values, and drops
// empty keys.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
