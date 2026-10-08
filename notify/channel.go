package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is an adapter for a notification channel. Implementations must be
// **stateless**: a single instance is reused concurrently across multiple channel
// configs, so all credentials are passed in via the cfg argument.
type Channel interface {
	// Kind returns the channel type identifier, which must match the key in the registry.
	Kind() string
	// Validate is called when saving a config to check required fields and formats.
	// The returned error is shown directly to the configurator, so the message must
	// say which field is missing rather than a generic "invalid config".
	Validate(cfg map[string]any) error
	// Send delivers a message once and returns the number of items actually
	// delivered and an error.
	//
	// Why return a count: every platform has a message length limit, and a digest
	// that cannot fit the whole batch gets truncated. If the caller unconditionally
	// marks the entire batch delivered, the truncated items silently vanish — they
	// are absent from the message and show as successful in the delivery history,
	// so nothing reveals that the vulnerability was never sent. By returning kept,
	// the caller marks only the first kept items and leaves the rest for the next
	// batch.
	//
	// A returned error indicates delivery failure; *PermanentError means it should
	// not be retried. On failure kept is meaningless and the caller should ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the channel's officially recommended per-minute
	// delivery limit, used as the default rate limit for new channel instances.
	// Returning 0 means no known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the names of the config keys that hold credentials. Their
	// values are masked when echoed by the API, and a masked value received on
	// update preserves the stored value. Only the implementation knows which fields
	// count as credentials (the whole Webhook URL is a credential for WeCom, while
	// for DingTalk only the secret inside it is), so this knowledge must come from
	// the channel and cannot be guessed by the upper layer.
	SecretKeys() []string
	// DestinationKeys returns the names of the config keys that determine where a
	// message is delivered.
	//
	// Like SecretKeys, this is security-related: the destination and the credentials
	// are two independent sets of fields. If "change only the address while keeping
	// the credentials" were allowed, anyone who can edit a channel config could send
	// the real stored credentials to a server they control, defeating the masking of
	// channel configs entirely. See PrepareConfigUpdate.
	DestinationKeys() []string
}

// registry is the channel registry. It deliberately uses an explicit literal rather
// than init() self-registration: the full set of channels is visible in one place,
// and a missing registration for a new channel surfaces at compile time rather than
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

// Kinds returns all supported channel types, sorted lexicographically (for a stable
// UI dropdown).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that should not be retried: bad
// credentials, target rejection, an invalid request body, and so on. Retrying only
// makes sense for transient faults (network jitter, rate limiting, peer 5xx);
// repeatedly backing off and retrying a permanent failure never succeeds and buries
// the real error in the retry logs.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. It returns nil when err is nil, so it
// can be written as `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err carries a permanent-failure marker anywhere in its
// chain.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- Config-reading helpers ----
//
// Channel configs come from a database JSONB column; after encoding/json
// deserialization they are map[string]any, with numbers as float64 and arrays as
// []any. These helpers normalize that conversion and tolerate the type drift caused
// by users leaving fields blank in the UI (e.g. entering a port as a string).

// cfgString reads a string config value and trims surrounding whitespace, which is
// easy to pick up when copy-pasting from a web form.
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

// cfgInt reads an integer config value, accepting both float64 (the JSON default)
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

// cfgBool reads a boolean config value, accepting the strings "true"/"1".
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

// cfgStrings reads a string-array config value, trimming whitespace and dropping
// empty entries.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// Also accept a single string, to make form submission with one value easy.
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

// cfgMap reads a string-map config value (e.g. custom HTTP headers), trimming both
// keys and values and dropping empty keys.
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
