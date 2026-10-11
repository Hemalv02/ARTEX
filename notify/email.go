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

// emailDialTimeout / emailSessionTimeout constrain connection
// establishment and the entire SMTP session respectively. net/smtp
// itself has no timeout mechanism; without these two, a stuck peer
// would leave the delivery goroutine hanging forever — and the
// dispatcher handles things serially in a single goroutine, which
// would bring the entire notification system to a halt.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel implements SMTP email delivery.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// Email has no platform rate limit, but it should not be used to
// flood; provide a generous default.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// Only the password is masked. The SMTP host, account, and
// recipients are not secrets; masking them would only make editing
// more painful.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port decide which server the password is handed to; tls
// decides whether the transmission is encrypted. Any change in any
// of the three requires restating the password — this also makes
// "turn off TLS" require an explicit credential reset, rather than
// being a one-click operation.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("missing SMTP server address")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("invalid SMTP port (should be 1-65535)")
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

	// STARTTLS: upgrade when the peer supports it. Credentials must
	// not be sent on a plaintext session (see the auth note below).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS failed: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth will refuse to send credentials over an
			// unencrypted connection (unless the target is localhost).
			// This is **correct** security behavior and cannot be
			// bypassed, but the reason needs to be made clear —
			// otherwise the user just sees "unencrypted connection"
			// and does not know what to do.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("refusing to send credentials: connection is not encrypted. Please enable TLS, or use port 465 (implicit TLS), or check 'enable TLS' (%w)", err))
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
	// Quit failure does not affect the fact that "the email has been
	// received by the server", so its error is ignored.
	_ = client.Quit()
	// Email has no length truncation (the full HTML body is sent), so
	// the whole batch counts as delivered.
	return len(m.Items), nil
}

// emailDial establishes an SMTP connection.
//
// implicitTLS=true uses the "TLS on connect" form, e.g. port 465;
// false uses 25/587 with a plaintext connection and then STARTTLS.
// The two cannot be mixed: sending a plaintext greeting to port 465
// gets the connection closed immediately.
//
// The session deadline is set at connection time (not added later),
// because net/smtp's Client hides the underlying connection in
// unexported fields, and external code cannot get to it; once the
// connection is handed over, only a pre-set deadline can act as a
// backstop. This also covers the handshake-phase blocking.
// Control attaches blockInternalDial, the same guard the HTTP-family
// channels share. Without it, SMTP would be the gap in the entire
// SSRF protection: filling host with 169.254.169.254 or 127.0.0.1
// would connect directly, and when smtp.NewClient's handshake failed
// it would wrap the peer's returned line into the error, echoed back
// through last_error by the delivery history endpoint, forming a
// half-blind read primitive; the timing difference between
// "connection refused" and "timeout" could also be used to probe
// ports. The dial stage is the final point of effect, and it also
// covers DNS rebinding.
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

// smtpStageError uses the SMTP reply code to split a stage's failure
// into "retryable" and "permanent failure".
//
// Why the distinction is required: SMTP 4xx and 5xx have completely
// different semantics —
//   - 4xx (450 greylist, 451 local error, 452 storage insufficient)
//     is a **temporary** rejection; the proper response is to retry
//     later; greylist in particular is almost always encountered on
//     the first delivery.
//   - 5xx (550 user not found, 553 invalid address) is a permanent
//     rejection; retrying is pointless.
//
// If everything is judged permanent, a server with greylisting would
// make **every** push fall into failed after the first attempt — and
// these are exactly the failures where automatic retry should work
// best. The reply code is taken from the first three digits of the
// error text; if the code cannot be obtained it is treated as
// retryable (better to try once more than to call a possible
// transient fault dead).
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode pulls the leading three-digit reply code from an
// SMTP error text; returns 0 if none can be found. net/smtp does not
// export the error code field, so it can only be read from the text;
// the format is "450 4.7.1 ...".
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

// buildEmailMessage assembles a complete RFC 5322 email message.
//
// The body is base64-encoded for two reasons: first, SMTP requires
// each line to be no longer than 1000 bytes, and HTML bodies (especially
// in summary emails) can easily contain overlong lines; second,
// base64 by nature never produces a line starting with ".", so SMTP
// dot-stuffing is unnecessary.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// CJK subjects must be RFC 2047 encoded, otherwise the client
	// displays them as garbled text.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// Email has no hard length limit, so the body is not truncated.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 wraps at 76 characters, per RFC 2045.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
