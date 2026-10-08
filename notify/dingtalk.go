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
// Platform characteristics (which drive the implementation choices):
//   - A single bot is rate-limited to 20 messages/minute; excess messages are
//     silently dropped (HTTP can still be 200), so rate limiting must happen client
//     side, see DefaultRatePerMin.
//   - Security settings are three-way: signing / custom keyword / IP allowlist.
//     Signing is the only option that does not depend on message content, so only
//     signing is supported (a bare webhook with none of the three is also supported).
//   - Both success and failure return HTTP 200, distinguished by errcode in the body —
//     not checking errcode would record a failed delivery as successful.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk's Webhook URL carries an access_token and is itself a credential, so it is
// masked as a whole.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The destination is the DingTalk Webhook URL itself; changing it requires re-stating
// the signing secret for the new address.
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

// Send delivers a message once. Uses ActionCard (with a button) when there is a link
// back and a single item; otherwise uses markdown.
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
	// DingTalk's markdown body has no explicit byte limit, but a limit is still applied
	// as protection against an abnormally bloated evidence field.
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
	// DingTalk hides business errors in a 200 response.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 is signature-verification failure and 310000 is keyword mismatch —
		// both are config errors and won't self-heal on retry.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends timestamp and sign parameters to the webhook per the
// official signing rules.
//
// Rule: the string to sign = timestamp + "\n" + secret, and the HMAC-SHA256 **key is
// also the secret**; the result is base64-encoded then URL-encoded. timestamp is in
// milliseconds. When secret is empty the hook is returned unchanged, to support bots
// with signing disabled.
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
		// Don't pass err through: url.Parse's error text carries the full address
		// (including the access_token).
		return "", fmt.Errorf("failed to parse Webhook URL: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL checks that the address is usable and the scheme is supported, and
// performs an internal-address check for literal IP targets.
//
// Two points of care:
//
//  1. **Error messages must be redacted.** url.Parse itself returns a *url.Error whose
//     Error() carries the **full original address**, and for the platforms this feature
//     supports the address has credentials embedded (DingTalk access_token, WeCom key,
//     Telegram bot token, Feishu hook id). Previously this returned `err` directly, so
//     the "invalid address format" error leaked the credentials into the test-send
//     400 response, the last_error stored for each delivery, server logs, and the
//     delivery-history API.
//
//  2. **A literal IP is checked as internal directly**, while hostnames are left to the
//     dial phase (blockInternalDial is the final enforcement point and also covers DNS
//     rebinding). Doing it here yields a hint when saving the config, rather than
//     waiting for the first delivery failure.
//
// Restricting the scheme is defensive: file:// / gopher:// and the like would make
// http.Client behave unexpectedly (already blocked by the scheme check, but there's no
// reason to leave that surface open).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("address could not be parsed (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https are supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing hostname")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("refusing to deliver to a loopback/link-local address %s (if you really need to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
