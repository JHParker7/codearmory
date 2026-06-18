package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func init() { RegisterNotifier(webhookNotifier{}) }

// webhookNotifier delivers messages as a generic JSON POST to any HTTP endpoint.
// It is the "etc" catch-all integration: anything that can receive a webhook
// (PagerDuty events API, a custom bridge, Discord, …) can be wired up without a
// dedicated provider.
type webhookNotifier struct{}

func (webhookNotifier) Info() ProviderInfo {
	return ProviderInfo{
		Type:  ChannelTypeWebhook,
		Label: "Generic Webhook",
		Fields: []ConfigField{
			{Key: "url", Label: "Endpoint URL", Required: true},
			{Key: "auth_header", Label: "Authorization header", Secret: true,
				Help: "Optional value sent as the Authorization header, e.g. 'Bearer xxx'"},
		},
	}
}

func (webhookNotifier) Validate(config map[string]string) error {
	if !strings.HasPrefix(config["url"], "https://") && !strings.HasPrefix(config["url"], "http://") {
		return fmt.Errorf("url must be an http(s) URL")
	}
	return nil
}

func (webhookNotifier) Send(ctx context.Context, config map[string]string, msg Message) error {
	raw, err := json.Marshal(map[string]string{
		"subject": msg.Subject,
		"body":    msg.Body,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config["url"], bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if h := config["auth_header"]; h != "" {
		req.Header.Set("Authorization", h)
	}

	resp, err := webhookClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}
