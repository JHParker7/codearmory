package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Provider Send implementations use the package-level clients; the real ones
	// are built in main(). Give tests plain clients. The webhook/slack tests hit
	// loopback httptest servers, which the production SSRF guard would block, so
	// the test webhookClient is deliberately unguarded; the guard itself is
	// covered by TestWebhookClientBlocksLoopback / TestIsBlockedIP.
	httpClient = &http.Client{}
	webhookClient = &http.Client{}
	os.Exit(m.Run())
}

func TestProvidersRegistered(t *testing.T) {
	for _, typ := range []string{ChannelTypeSlack, ChannelTypeEmail, ChannelTypeWebhook} {
		if _, ok := getNotifier(typ); !ok {
			t.Errorf("provider %q not registered", typ)
		}
	}
	if got := len(providerInfos()); got < 3 {
		t.Errorf("expected at least 3 providers, got %d", got)
	}
	if _, ok := getNotifier("does-not-exist"); ok {
		t.Error("unexpected provider for unknown type")
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		typ     string
		config  map[string]string
		wantErr bool
	}{
		{"slack ok", ChannelTypeSlack, map[string]string{"webhook_url": "https://hooks.slack.com/x"}, false},
		{"slack missing required", ChannelTypeSlack, map[string]string{}, true},
		{"slack bad url", ChannelTypeSlack, map[string]string{"webhook_url": "ftp://nope"}, true},
		{"slack unknown field", ChannelTypeSlack, map[string]string{"webhook_url": "https://x", "bogus": "1"}, true},
		{"email ok", ChannelTypeEmail, map[string]string{"host": "smtp.x", "from": "a@x", "to": "b@x"}, false},
		{"email missing to", ChannelTypeEmail, map[string]string{"host": "smtp.x", "from": "a@x"}, true},
		{"email bad port", ChannelTypeEmail, map[string]string{"host": "smtp.x", "from": "a@x", "to": "b@x", "port": "abc"}, true},
		{"webhook ok", ChannelTypeWebhook, map[string]string{"url": "https://x/y"}, false},
		{"webhook bad url", ChannelTypeWebhook, map[string]string{"url": "nope"}, true},
		{"unknown type", "telegram", map[string]string{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.typ, tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRedactConfig(t *testing.T) {
	cfg := map[string]string{"webhook_url": "https://secret", "channel": "#ops"}
	out := redactConfig(ChannelTypeSlack, cfg)
	if out["webhook_url"] != redacted {
		t.Errorf("secret webhook_url not redacted: %q", out["webhook_url"])
	}
	if out["channel"] != "#ops" {
		t.Errorf("non-secret channel altered: %q", out["channel"])
	}
	// The original map must be untouched.
	if cfg["webhook_url"] != "https://secret" {
		t.Error("redactConfig mutated the input map")
	}
}

func TestMergeConfigKeepsRedactedSecret(t *testing.T) {
	existing := map[string]string{"webhook_url": "https://original", "channel": "#old"}
	incoming := map[string]string{"webhook_url": redacted, "channel": "#new"}
	merged := mergeConfig(ChannelTypeSlack, existing, incoming)
	if merged["webhook_url"] != "https://original" {
		t.Errorf("redacted secret should preserve existing value, got %q", merged["webhook_url"])
	}
	if merged["channel"] != "#new" {
		t.Errorf("non-secret field should update, got %q", merged["channel"])
	}
}

func TestSlackSend(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := slackNotifier{}.Send(context.Background(),
		map[string]string{"webhook_url": srv.URL, "channel": "#ops"},
		Message{Subject: "Hi", Body: "world"})
	if err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if text, _ := gotBody["text"].(string); !strings.Contains(text, "Hi") || !strings.Contains(text, "world") {
		t.Errorf("unexpected slack text payload: %q", gotBody["text"])
	}
	if gotBody["channel"] != "#ops" {
		t.Errorf("channel override not forwarded: %v", gotBody["channel"])
	}
}

func TestSlackSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := slackNotifier{}.Send(context.Background(),
		map[string]string{"webhook_url": srv.URL}, Message{Body: "x"})
	if err == nil {
		t.Error("expected error on non-2xx response")
	}
}

func TestWebhookSend(t *testing.T) {
	var gotAuth, gotCT string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	err := webhookNotifier{}.Send(context.Background(),
		map[string]string{"url": srv.URL, "auth_header": "Bearer t0k"},
		Message{Subject: "s", Body: "b"})
	if err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if gotAuth != "Bearer t0k" {
		t.Errorf("auth header not sent, got %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type not json, got %q", gotCT)
	}
	if gotBody["subject"] != "s" || gotBody["body"] != "b" {
		t.Errorf("unexpected webhook body: %+v", gotBody)
	}
}

func TestEmailRecipients(t *testing.T) {
	got := emailRecipients(" a@x.com ,b@x.com,, c@x.com ")
	want := []string{"a@x.com", "b@x.com", "c@x.com"}
	if len(got) != len(want) {
		t.Fatalf("got %d recipients, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("recipient[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildEmailMessageHeaderInjection(t *testing.T) {
	// A CR/LF in the subject must not break out into a new header line; the
	// RFC 2047 encoding hex-escapes the newline inside the encoded-word.
	msg := Message{
		Subject: "Hi\r\nBcc: victim@evil.com\r\nContent-Type: text/html",
		Body:    "hello",
	}
	out, err := buildEmailMessage("a@x.com", []string{"b@x.com"}, msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "\r\nBcc:") {
		t.Errorf("subject injected a raw Bcc header line:\n%q", out)
	}
	// No header line (in the block before the blank-line body separator) may be
	// an attacker-introduced header.
	headers, _, _ := strings.Cut(out, "\r\n\r\n")
	for line := range strings.SplitSeq(headers, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") || strings.HasPrefix(line, "Content-Type: text/html") {
			t.Errorf("injected header line present: %q", line)
		}
	}
}

func TestBuildEmailMessageRejectsBadAddresses(t *testing.T) {
	if _, err := buildEmailMessage("a@x.com\r\nBcc: e@evil.com", []string{"b@x.com"}, Message{}); err == nil {
		t.Error("expected error for CRLF in from address")
	}
	if _, err := buildEmailMessage("a@x.com", []string{"b@x.com\r\nBcc: e@evil.com"}, Message{}); err == nil {
		t.Error("expected error for CRLF in recipient address")
	}
}

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1",
		"10.0.0.5", "172.16.3.4", "192.168.1.1",
		"169.254.169.254", // cloud metadata endpoint
		"fe80::1", "fc00::1",
		"0.0.0.0", "::",
		"::ffff:127.0.0.1", // IPv4-mapped loopback
	}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("expected %s to be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2001:4860:4860::8888"}
	for _, s := range allowed {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("expected %s to be allowed", s)
		}
	}
}

func TestWebhookClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The production-configured guarded client must refuse the loopback httptest
	// server, proving the SSRF dial guard is wired in.
	guarded := initWebhookClient()
	req, err := http.NewRequest(http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if resp, err := guarded.Do(req); err == nil {
		resp.Body.Close()
		t.Error("expected the guarded webhook client to refuse a loopback URL (SSRF guard)")
	}
}

func TestRegisterNotifierDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate provider registration")
		}
	}()
	RegisterNotifier(slackNotifier{}) // already registered → must panic
}
