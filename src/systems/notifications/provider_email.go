package main

import (
	"context"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
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

	// smtp.SendMail is blocking with no context support; enforce the caller's
	// deadline by running it on a goroutine and selecting on ctx.
	done := make(chan error, 1)
	go func() {
		done <- smtp.SendMail(addr, auth, from, recipients, []byte(body))
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
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
	return strings.Join([]string{
		"From: " + from,
		"To: " + strings.Join(recipients, ", "),
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		msg.Body,
	}, "\r\n"), nil
}
