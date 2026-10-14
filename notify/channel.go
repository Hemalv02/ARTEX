package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is an adapter for a notification channel. Implementations must be
// **stateless**: a single instance is reused concurrently across many channel
// configs, and credentials are always passed in via the cfg parameter.
type Channel interface {
	// Kind returns the channel type identifier; it must match the key in the registry.
	Kind() string
	// Validate is called when saving a config to check required fields and format.
	// The returned error is shown directly to the person configuring the channel,
	// so the wording should say "which field is missing" rather than a vague
	// "invalid configuration".
	Validate(cfg map[string]any) error
	// Send delivers one message and returns the **number of items actually
	// delivered** along with an error.
	//
	// Why return a count: every platform has a message-length limit, and a digest
	// message that can't fit the whole batch gets truncated. If the caller
	// unconditionally marks the whole batch as delivered, the truncated items just
	// vanish — they aren't in the message, the delivery history shows success, and
	// there is nowhere to notice that a finding was never sent. By returning kept,
	// the caller marks only the first kept items and leaves the rest for the next batch.
	//
	// A returned error means delivery failed; a *PermanentError means it should not
	// be retried. On failure kept is meaningless and the caller should ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the channel's officially recommended per-minute
	// delivery limit, used as the default rate limit when creating a new channel
	// instance. Returns 0 if there is no known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the config key names that count as credentials for this
	// channel. These keys' values are masked when the API echoes them back, and a
	// masked value received on update means keep the stored original. Only the
	// implementation itself knows which fields count as credentials (for WeCom the
	// entire Webhook URL is the credential, while for DingTalk it's only the secret
	// within it), so this knowledge must be provided by the channel and cannot be
	// guessed by the layer above.
	SecretKeys() []string
	// DestinationKeys returns the config key names that decide "where the message
	// is sent" for this channel.
	//
	// Like SecretKeys, this is security-relevant: the destination address and the
	// credentials are two independent sets of fields. If we allowed "change only the
	// address, keep the credentials as-is", anyone who can change a channel config
	// could make the server send the real stored credentials to a server they
	// control, and the masking of the channel config would be completely
	// meaningless. See PrepareConfigUpdate.
	DestinationKeys() []string
}

// registry is the channel registry. Deliberately an explicit literal rather than
// init()-based self-registration: this way "which channels exist" can be seen in
// one place, and adding a channel surfaces omissions at compile time instead of
// relying on a runtime side effect.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get returns the channel implementation for a type.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns all supported channel types, sorted lexicographically (for a
// stable UI dropdown).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that should not be retried: bad
// credentials, destination rejection, an invalid request body, etc. Retrying only
// makes sense for transient failures (network jitter, rate limiting, a 5xx from
// the peer); repeatedly backing off and retrying a permanent failure won't
// succeed and just buries the real error in the retry log.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. Returns nil when err is nil, so it
// can be written as `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether the err chain carries a permanent-failure marker.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- config-reading helpers ----
//
// Channel configs come from a JSONB column in the database; after
// encoding/json deserialization they are a map[string]any, with all numbers as
// float64 and arrays as []any. The helpers below unify this layer of conversion
// and tolerate the type drift caused by users leaving fields blank in the UI
// (such as filling a port as a string).

// cfgString reads a string config item, trimming leading/trailing whitespace —
// copy-pasting from a web form very easily picks it up.
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

// cfgInt reads an integer config item, accepting both float64 (the JSON default)
// and string sources.
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

// cfgBool reads a boolean config item, accepting the strings "true"/"1".
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

// cfgStrings reads a string-array config item, trimming whitespace and dropping
// empty strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// Also accept a single string, convenient for a form submission with just one value.
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

// cfgMap reads a string-map config item (such as custom HTTP headers), trimming
// whitespace on keys and values and dropping empty keys.
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
