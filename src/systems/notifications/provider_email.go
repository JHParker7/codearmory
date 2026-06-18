package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

func init() { RegisterNotifier(emailNotifier{}) }

// emailNotifier delivers messages over SMTP using the standard library. It uses
// STARTTLS automatically when the relay advertises it, and authenticates with
// PLAIN auth when a username is configured (omit for unauthenticated relays such
// as a local MailHog/Mailpit).
type emailNotifier struct{}

func (emailNotifier) Info() ProviderInfo {
	return ProviderInfo{
		Type:  ChannelTypeEmail,
		Label: "Email (SMTP)",
		Fields: []ConfigField{
			{Key: "host", Label: "SMTP host", Required: true},
			{Key: "port", Label: "SMTP port", Help: "Defaults to 587"},
			{Key: "username", Label: "Username", Help: "Omit for unauthenticated relays"},
			{Key: "password", Label: "Password", Secret: true},
			{Key: "from", Label: "From address", Required: true},
			{Key: "to", Label: "Recipients", Required: true, Help: "Comma-separated list of addresses"},
		},
	}
}

func (emailNotifier) Validate(config map[string]string) error {
	if p := config["port"]; p != "" {
		if _, err := strconv.Atoi(p); err != nil {
			return fmt.Errorf("port must be numeric")
		}
	}
	if len(emailRecipients(config["to"])) == 0 {
		return fmt.Errorf("at least one recipient is required")
	}
	return nil
}

// emailRecipients splits a comma-separated recipient list into trimmed,
// non-empty addresses.
func emailRecipients(to string) []string {
	var out []string
	for r := range strings.SplitSeq(to, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func (emailNotifier) Send(ctx context.Context, config map[string]string, msg Message) error {
	port := config["port"]
	if port == "" {
		port = "587"
	}
	addr := net.JoinHostPort(config["host"], port)
	from := config["from"]
	recipients := emailRecipients(config["to"])

	body, err := buildEmailMessage(from, recipients, msg)
	if err != nil {
		return err
	}

	var auth smtp.Auth
	if config["username"] != "" {
		auth = smtp.PlainAuth("", config["username"], config["password"], config["host"])
	}

	return sendMailGuarded(ctx, addr, config["host"], auth, from, recipients, []byte(body))
}

// sendMailGuarded reimplements smtp.SendMail over an SSRF-guarded, context-aware
// dialer so the email provider gets the same protection as the webhook/Slack
// clients. The dialer's Control hook (ssrfGuardControl) rejects any connection to
// a non-public IP — loopback, private, link-local, or the cloud metadata endpoint
// — against the *resolved* address, so a user-supplied SMTP host can no longer be
// used to reach internal services (SSRF). DialContext is bound to ctx and the
// connection inherits the caller's deadline, so a hung relay can neither block
// past the timeout nor leak a goroutine the way the old smtp.SendMail call did.
func sendMailGuarded(ctx context.Context, addr, host string, auth smtp.Auth, from string, to []string, msg []byte) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: ssrfGuardControl}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	// Bound the whole SMTP conversation by ctx: apply the deadline and tear the
	// connection down if ctx is cancelled mid-exchange.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(auth); err != nil {
				return err
			}
		} else {
			// Credentials were configured but the relay doesn't offer AUTH — fail
			// loudly rather than silently sending unauthenticated (matches stdlib
			// smtp.SendMail, which errors instead of downgrading).
			return fmt.Errorf("smtp: server %q does not support AUTH but credentials were provided", host)
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(msg); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// sanitizeEmailBody normalizes body content before it is embedded into an email.
// This reduces injection risk from untrusted input by removing NUL bytes and
// canonicalizing line endings to CRLF for RFC 5322 message bodies.
func sanitizeEmailBody(body string) string {
	body = strings.ReplaceAll(body, "\x00", "")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	return body
}

// buildEmailMessage assembles the RFC 5322 message and guards against email
// header injection. The subject is RFC 2047-encoded, which hex-escapes any CR/LF
// inside an encoded-word so it can never terminate the Subject line; the from and
// recipient addresses are rejected outright if they contain CR/LF. Without this a
// caller-controlled subject like "Hi\r\nBcc: victim@x\r\nContent-Type: text/html"
// could inject extra headers or a forged HTML body.
func buildEmailMessage(from string, recipients []string, msg Message) (string, error) {
	if strings.ContainsAny(from, "\r\n") {
		return "", fmt.Errorf("invalid from address: contains line breaks")
	}
	for _, rcpt := range recipients {
		if strings.ContainsAny(rcpt, "\r\n") {
			return "", fmt.Errorf("invalid recipient address: contains line breaks")
		}
	}
	subject := msg.Subject
	if subject == "" {
		subject = "Notification"
	}
	body := sanitizeEmailBody(msg.Body)
	return strings.Join([]string{
		"From: " + from,
		"To: " + strings.Join(recipients, ", "),
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		body,
	}, "\r\n"), nil
}
