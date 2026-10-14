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

// emailDialTimeout / emailSessionTimeout bound connection establishment and the
// entire SMTP session respectively. net/smtp has no timeout mechanism of its own;
// without these two limits a stuck peer would hang the delivery goroutine forever
// — and since the dispatcher processes serially on a single goroutine, that would
// stall the whole notification system.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel implements SMTP email delivery.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// Email has no platform rate limit, but it shouldn't be used to spam; give a lenient default.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// Only mask the password. The SMTP host, account, and recipients are not secrets;
// masking them would only make editing cumbersome.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port decide which server the password is handed to; tls decides whether the
// transport is encrypted. Any change to these three requires re-entering the password
// — which also forces "turning TLS off" to be an explicit step that carries the
// credential, rather than a casual edit.
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

	// STARTTLS: upgrade if the peer supports it. Credentials must not be sent over a
	// plaintext session (see the auth note below).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS failed: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth refuses to send credentials over an unencrypted connection
			// (unless the target is localhost). This is the **correct** security behavior
			// and must not be bypassed, but the reason needs to be spelled out clearly —
			// otherwise the user just sees "unencrypted connection" and has no idea what to do.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("refusing to send credentials: the connection is not encrypted. Enable TLS, switch to port 465 (implicit TLS), or check the \"Enable TLS\" option (%w)", err))
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
	// A Quit failure doesn't change the fact that the server already accepted the email,
	// so its error is ignored.
	_ = client.Quit()
	// Email has no length truncation (the entire HTML body is sent), so the whole batch
	// counts as delivered.
	return len(m.Items), nil
}

// emailDial establishes an SMTP connection.
//
// implicitTLS=true uses the "TLS as soon as connected" style of port 465; false uses
// plaintext connection on 25/587 followed by STARTTLS. The two cannot be mixed: sending
// a plaintext greeting to port 465 gets the connection dropped outright.
//
// The session deadline is set **at connection time** (rather than patched on afterward),
// because net/smtp's Client hides the underlying connection in an unexported field that
// is unreachable from outside; once the connection has been handed over, only the
// pre-set deadline can backstop it. This also incidentally covers blocking during the
// handshake phase.
// Hanging blockInternalDial on Control shares the same guard as the HTTP-family channels.
// Without it SMTP would be the hole in the whole SSRF defense: a host of
// 169.254.169.254 or 127.0.0.1 could be connected to directly, and when smtp.NewClient's
// handshake fails it wraps the line returned by the peer into the error, which is echoed
// back through last_error by the delivery-history API — a semi-blind read primitive; the
// timing difference between "connection refused vs. timeout" can also be used to probe
// ports. The dial phase is the final point of enforcement, and it also covers DNS rebinding.
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

// smtpStageError classifies a failure in a given stage as "retryable" or "permanent"
// based on the SMTP reply code.
//
// Why the distinction is essential: SMTP's 4xx and 5xx have completely different
// semantics —
//   - 4xx (450 greylisting, 451 local error, 452 insufficient storage) is a **temporary**
//     rejection; the proper response is to retry later. Greylisting in particular is hit
//     on almost every first delivery attempt.
//   - 5xx (550 no such user, 553 invalid address) is a permanent rejection where retrying
//     is pointless.
//
// If everything were treated as a permanent failure, a mail server with greylisting
// enabled would send **every single** notification into failed after the first attempt
// — yet this is exactly the kind of failure automatic retry is meant to handle.
// The reply code is taken from the first three digits of the error text; when no code
// can be read it is treated as retryable (better to try once more than to condemn a
// possibly transient failure just because it couldn't be parsed).
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode extracts the leading three-digit reply code from SMTP error text,
// returning 0 if none can be found. net/smtp does not export an error-code field, so it
// can only be read from the text; the format is "450 4.7.1 ...".
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
// The body is base64-encoded for two reasons: first, SMTP mandates that a single line not
// exceed 1000 bytes, and HTML bodies (especially digest emails) easily produce overlong
// lines; second, base64 never naturally produces a line starting with ".", which avoids
// the hassle of SMTP dot-escaping.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// A non-ASCII subject must be RFC 2047 encoded, or clients will display it as garbled text.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// Email has no hard length limit, so the body is not truncated.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// Wrap base64 at 76 characters per line, per RFC 2045.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
