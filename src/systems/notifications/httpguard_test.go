package main

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

// TestScrubURLError ensures a transport *url.Error (which embeds the request URL,
// and thus a channel's secret webhook_url/token) is stripped before it can reach a
// stored last_error or an API response, while the underlying cause is preserved.
func TestScrubURLError(t *testing.T) {
	secretURL := "https://hooks.slack.com/services/T0/B0/SUPERSECRETTOKEN"
	ue := &url.Error{
		Op:  "Post",
		URL: secretURL,
		Err: errors.New("dial tcp 1.2.3.4:443: connect: connection refused"),
	}
	got := scrubURLError(ue).Error()
	if strings.Contains(got, "SUPERSECRETTOKEN") || strings.Contains(got, secretURL) {
		t.Errorf("scrubbed error still leaks the URL/token: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("scrubbed error should keep the underlying cause, got: %q", got)
	}

	// A non-url error must pass through unchanged.
	plain := errors.New("plain failure")
	if scrubURLError(plain) != plain {
		t.Error("non-url error should pass through unchanged")
	}
}

// TestEmailProviderSSRFGuard proves the email/SMTP path is wired through the same
// SSRF dial guard as the webhook/Slack clients: a user-supplied SMTP host that
// resolves to a non-public address (loopback / cloud metadata / RFC1918) is
// refused at dial time, before any SMTP exchange.
func TestEmailProviderSSRFGuard(t *testing.T) {
	for _, host := range []string{"127.0.0.1:25", "169.254.169.254:25", "10.0.0.5:587"} {
		hostOnly, _, _ := strings.Cut(host, ":")
		err := sendMailGuarded(context.Background(), host, hostOnly, nil, "a@x.com", []string{"b@x.com"}, []byte("test"))
		if err == nil {
			t.Errorf("email send to non-public host %q should be refused (SSRF guard)", host)
			continue
		}
		if !strings.Contains(err.Error(), "non-public") {
			t.Errorf("host %q: error should come from the SSRF guard, got: %v", host, err)
		}
	}
}
