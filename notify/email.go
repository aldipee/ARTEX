package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout constrain connection establishment and the
// whole SMTP session respectively. net/smtp has no timeout mechanism of its own; without
// these two, a stuck peer would leave the delivery goroutine hung forever — and since
// the dispatcher processes serially in a single goroutine, that stalls the entire
// notification system.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel implements SMTP email delivery.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// Email has no platform rate limit, but it shouldn't be used to spam; use a relaxed
// default.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// Only the password is masked. The SMTP host, account, and recipients are not secrets;
// masking them would only make editing more cumbersome.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port determine which server the password is handed to; tls determines whether
// the transport is encrypted. Any change among the three requires re-stating the
// password — which also makes "turn off TLS" explicitly carry the credentials instead
// of being a one-click change.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("missing SMTP server address")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("invalid SMTP port (must be 1-65535)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("missing sender address")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("at least one recipient address is required")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// Upgrade to STARTTLS when the server supports it. Do not send credentials over a plaintext session (see auth below).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS failed: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth refuses to send credentials over an unencrypted connection (unless the
			// destination is localhost). This is correct security behavior and must not be bypassed,
			// but the error should explain how to fix it instead of only saying "unencrypted connection."
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("refusing to send credentials over an unencrypted connection. Enable TLS, use port 465 (implicit TLS), or check the Enable TLS option (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP authentication failed: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("sender %s was rejected", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("recipient %s was rejected", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA failed: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("failed to write email body: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("failed to submit email: %w", err)
	}
	// Ignore errors from Quit; they do not change the fact that the server accepted the email.
	_ = client.Quit()
	// Emails are not length-truncated (the full HTML body is sent); the whole batch counts as delivered.
	return len(m.Items), nil
}

// emailDial opens an SMTP connection.
//
// implicitTLS=true uses immediate TLS (such as port 465); false starts in plaintext on port
// 25/587 and then upgrades with STARTTLS. Do not mix them: a plaintext greeting on port 465 is
// immediately disconnected.
//
// Set the session deadline while dialing, not afterward: net/smtp.Client hides the underlying
// connection in an unexported field, so it cannot be reached once handed off. The pre-set
// deadline also covers a stalled handshake. Control uses the same blockInternalDial guard as
// HTTP channels. Without it, SMTP would be an SSRF gap: a host of 169.254.169.254 or 127.0.0.1
// could be reached directly. If smtp.NewClient's handshake failed, it would include the peer's
// response line in the error, which last_error exposes through delivery history. Timing
// differences between refusal and timeout could also probe ports. Dial-time checks are the
// final enforcement point and cover DNS rebinding too.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to SMTP server: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP handshake failed: %w", err)
	}
	return client, nil
}

// smtpStageError classifies an SMTP stage failure as retryable or permanent based on its reply code.
//
// This distinction matters because SMTP 4xx and 5xx have different meanings:
//   - 4xx (450 greylisting, 451 local error, 452 insufficient storage) are temporary rejections
//     and should be retried later. Greylisting is common on the first delivery attempt.
//   - 5xx (550 user does not exist, 553 invalid address) are permanent rejections; retries are pointless.
//
// If every response were permanent, a server using greylisting would mark every notification
// failed after the first attempt — exactly where automatic retries are useful. Read the first
// three digits of the error text as the reply code. If no code can be parsed, treat it as retryable:
// one extra attempt is preferable to permanently rejecting a possibly transient error.
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode reads the leading three-digit reply code from SMTP error text, returning 0 if absent.
// net/smtp does not expose a reply-code field, so parse the text (for example, "450 4.7.1 ...").
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage assembles a complete RFC 5322 email.
//
// Base64-encode the body for two reasons: SMTP limits lines to 1,000 bytes, and HTML bodies
// (especially digest emails) can easily exceed that; also, base64 never produces a line starting
// with ".", avoiding SMTP dot-stuffing.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// Encode non-ASCII subjects with RFC 2047 or mail clients may display them incorrectly.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// Emails have no hard length limit, so do not truncate the body.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// Wrap base64 at 76 characters per line as required by RFC 2045.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
