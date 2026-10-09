package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets controls whether notifications may be sent to loopback/link-local addresses.
//
// Deny these by default. IM bots and public mail servers should not use these address ranges,
// but they can reach sensitive services: another process's admin port or cloud metadata
// endpoints (169.254.169.254, which can expose instance credentials). Although an administrator
// configures the destination, a reused XSS/CSRF admin session or another person sharing the JWT
// could change the config and read the response: doJSON stores the first 200 bytes of 4xx/5xx
// response bodies in last_error, and the delivery-history API returns it, creating a semi-blind read.
//
// However, a local SMTP relay (such as postfix on 127.0.0.1:25) is common for self-hosted mail,
// so blocking it unconditionally would break legitimate setups. Provide an explicit escape hatch
// instead of a hard-coded allowlist: set ARTEX_NOTIFY_ALLOW_LOCAL=1 to allow it.
//
// Export AllowLocalTargetsEnv so tests can enable it explicitly. Tests in this and the server
// package use httptest receivers on 127.0.0.1 and would otherwise be blocked by the guard.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP is in a range blocked by default.
//
// Block only loopback, link-local (including cloud metadata 169.254.169.254), unspecified, and
// multicast addresses. Do not block RFC1918 private networks: self-hosted Mattermost/SMTP relays
// on a LAN are legitimate and common, and blocking them would make this feature unusable in real
// deployments. This tradeoff is deliberate: block sensitive targets without breaking normal setups.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// Unmap IPv4-mapped IPv6 (such as ::ffff:127.0.0.1) before checking, or it could bypass the guard.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is an http.Transport dialer's Control hook that checks the destination
// address when the connection is established.
//
// Check at dial time rather than only when saving the config because this is the final enforcement
// point. It also covers two ways to bypass config validation: DNS rebinding (resolves to a public
// IP during validation but a private IP at connection time) and redirects (cross-host redirects
// are rejected, but same-host redirects can still change the path).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("could not resolve destination address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("refusing to deliver to loopback/link-local address %s (to send to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport adds only a dial guard to the default Transport.
// Clone preserves all default tuning (connection pools, HTTP/2, timeouts, proxy, etc.)
// so adding the check does not change other behavior.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is shared by all notification channels.
//
// Deliberately do not reuse the project's global egress proxy (server-side GlobalProxy): it is
// for target traffic and is often an unstable tunnel. Notification delivery should not depend on
// network conditions at the target. IM notifications can connect directly. The timeout is 15s;
// a slower endpoint is already effectively unhealthy.
//
// Reject cross-host redirects: notification destinations are fixed endpoints and should not
// normally redirect to another host. Credentials for these services (DingTalk access_token,
// WeCom key, Telegram bot token) are in the URL, so following a cross-host redirect would expose
// them to the redirect target. Same-host redirects (such as adding a trailing slash) are allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing cross-host redirect (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit caps response-body reads. A misbehaving endpoint may return a huge body, while
// delivery history only needs the status code and a short description.
const respBodyLimit = 8 << 10

// doJSON sends one request and returns a size-limited response body.
//
// A nil payload sends an empty body (for GET requests or APIs that do not require one).
// Headers are added as-is for custom generic-webhook headers.
//
// Error classification is this function's main job: network failures and 5xx/408/429 are
// retryable; other 4xx responses are permanent failures. Retrying a 403 would only log the same
// error three times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// Serialization failures are local bugs (wrong configuration field types); retries cannot fix them.
			return nil, Permanent(fmt.Errorf("failed to build request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// Invalid URL, likely a user configuration error; this is permanent.
		// Do not return err directly because url.Parse's error text contains the full address.
		return nil, Permanent(fmt.Errorf("invalid request URL: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refusal, DNS failure, and timeout are usually transient; retry with backoff.
		// Redact the error before returning it. http.Client.Do returns *url.Error, whose Error()
		// includes the full URL, and credentials for these services are in the URL (DingTalk
		// access_token, WeCom key, Feishu hook ID, Telegram /bot<token>/). Without redaction,
		// credentials could leak through last_error in notification_deliveries (stored in plaintext),
		// the delivery-history API (bypassing config masking), server logs, and the 502 response
		// from the test-send endpoint.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limited) and 408 (timeout) are worth retrying; other 4xx errors indicate a
	// configuration or permission issue and will not improve with retries.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("remote endpoint rate-limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("remote endpoint error (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("remote endpoint rejected the request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet compresses a response body into one short line for error messages. Responses may
// contain newlines and excessive whitespace, which would break the delivery-history layout in last_error.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget reduces a delivery URL to "scheme://host/…" for error messages.
//
// This package has one deliberately aggressive URL-redaction rule: discard everything except
// the scheme and host. There is no general, safe way to determine which URL component is a credential:
//
//	DingTalk credentials are in the query      /robot/send?access_token=xxx
//	WeCom credentials are in the query         /cgi-bin/webhook/send?key=xxx
//	Feishu credentials are in the final path   /open-apis/bot/v2/hook/<hook_id>
//	Telegram credentials are in the middle     /bot<token>/sendMessage
//
// Keeping only supposedly useful components would require per-channel patches, and missing one
// would leak credentials. The host is enough to diagnose DNS, connection, or certificate issues;
// the masked suffix in channel configuration identifies the specific bot.
//
// On parse failure, return a fixed placeholder; never echo the original string.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparseable address)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError removes the address from a transport error and keeps only the underlying cause.
//
// *url.Error has the shape {Op, URL, Err}, and Error() includes the URL. Read its Err field
// directly instead of doing string replacement, which is less reliable across URL-encoded or
// escaped variants.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// Non-*url.Error values (such as redirect-policy errors) may contain an address too; redact them as well.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces http(s) URLs in text with their redacted forms.
//
// Used as a fallback for errors without structured fields (redirect-policy and third-party errors).
// Recognizes only http/https prefixes and splits on whitespace and quotes, which cannot occur in a URL.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
