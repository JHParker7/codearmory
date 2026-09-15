package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"os"
	"sort"
	"strings"
)

// deliver sends one rendered message to a channel through its provider. Best-effort: a
// non-2xx or transport error is returned so the caller can log a failed Delivery.
func deliver(ctx context.Context, c *Channel, message string, e Event) error {
	switch c.Provider {
	case "slack":
		return postJSON(ctx, c.Config["url"], map[string]string{"text": message})
	case "discord":
		return postJSON(ctx, c.Config["url"], map[string]string{"content": message})
	case "webhook":
		// A generic webhook gets the structured event plus the rendered text, so a
		// downstream can use either.
		return postJSON(ctx, c.Config["url"], map[string]any{"text": message, "event": e})
	case "email":
		return sendEmail(c.Config["to"], "CodeArmory: "+eventTitle(e), message)
	default:
		return fmt.Errorf("unknown provider %q", c.Provider)
	}
}

func postJSON(ctx context.Context, url string, body any) error {
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("channel has no url configured")
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("webhook %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// sendEmail delivers over SMTP configured entirely from the environment (SMTP_HOST,
// SMTP_PORT, SMTP_USER, SMTP_PASS, SMTP_FROM). With no SMTP_HOST it returns a clear
// "not configured" error rather than pretending to send — email is opt-in until an
// operator wires a relay.
func sendEmail(to, subject, body string) error {
	host := os.Getenv("SMTP_HOST")
	if host == "" {
		return fmt.Errorf("email delivery is not configured (set SMTP_HOST/PORT/USER/PASS/FROM)")
	}
	if strings.TrimSpace(to) == "" {
		return fmt.Errorf("channel has no 'to' address configured")
	}
	port := envOrDefault("SMTP_PORT", "587")
	from := envOrDefault("SMTP_FROM", "codearmory@localhost")
	msg := []byte("From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject + "\r\n\r\n" + body + "\r\n")
	var auth smtp.Auth
	if u := os.Getenv("SMTP_USER"); u != "" {
		auth = smtp.PlainAuth("", u, secret("SMTP_PASS"), host)
	}
	return smtp.SendMail(host+":"+port, auth, from, []string{to}, msg)
}

// eventTitle is a short one-line label for an event.
func eventTitle(e Event) string {
	if e.Subject != "" {
		return e.Type + " — " + e.Subject
	}
	return e.Type
}

// render turns an event into a human message. Provider-agnostic plain text with light
// markdown (Slack and Discord both render **bold**); the event's own data fields are
// appended so a reader sees the concrete PR number, ref, status, etc.
func render(e Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**", e.Type)
	if e.Source != "" {
		fmt.Fprintf(&b, " · %s", e.Source)
	}
	if e.Subject != "" {
		fmt.Fprintf(&b, "\n%s", e.Subject)
	}
	// Append the most useful data fields, sorted for stable output. Skip long/noisy values.
	if len(e.Data) > 0 {
		keys := make([]string, 0, len(e.Data))
		for k := range e.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			v := fmt.Sprint(e.Data[k])
			if v == "" || len(v) > 120 {
				continue
			}
			parts = append(parts, k+"="+v)
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "\n%s", strings.Join(parts, "  "))
		}
	}
	return b.String()
}
