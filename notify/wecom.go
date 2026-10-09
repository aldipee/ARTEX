package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard cap on WeCom group bot markdown content (bytes, not characters).
// It is the tightest limit across all six channels, and the main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements a WeCom group bot.
//
// Platform characteristics:
//   - Auth solely via the key on the URL, no signing supported — so the webhook address itself is the entire credential.
//   - markdown content caps at 4096 **bytes**; overlong messages are rejected whole (not truncated). Chinese is 3
//     bytes/char, meaning the body can only hold a bit over a thousand characters, so the client must truncate.
//   - Rate limit 20 messages/minute, likewise held up by client-side rate limiting.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has a single credential at the Webhook (the key on the URL), and it doesn't support signing —
// the whole address is the entire credential, there is no other field to mask.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has a single Webhook field, which is both the destination and the credential, so there is no "credential left
// behind after changing the address" to speak of.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook address is missing")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook address: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// A digest batch can be long (50 items × one line each + the prefix), easily exceeding 4096 bytes.
	// Truncate here rather than relying on the platform to error: rejection means the whole batch is lost, whereas
	// truncation at least delivers the first few items.
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
		// 45009 is interface-call-over-limit — the platform's rate-limit window rolls, so retrying after backoff is
		// effective, and it is explicitly classified as retryable. Reaching here means the client's rate_per_min is
		// set too aggressively; retrying is only a fallback, the real fix is to lower this channel's rate-limit value.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate-limited %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 is an invalid webhook key — permanent failure, retrying won't self-heal.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
