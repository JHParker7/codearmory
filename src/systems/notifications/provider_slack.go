package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func init() { RegisterNotifier(slackNotifier{}) }

// slackNotifier delivers messages to a Slack (or Slack-compatible, e.g.
// Mattermost) Incoming Webhook URL. No external SDK required — it POSTs the
// documented JSON payload.
type slackNotifier struct{}

func (slackNotifier) Info() ProviderInfo {
	return ProviderInfo{
		Type:  ChannelTypeSlack,
		Label: "Slack",
		Fields: []ConfigField{
			{Key: "webhook_url", Label: "Incoming Webhook URL", Required: true, Secret: true,
				Help: "Slack incoming webhook, e.g. https://hooks.slack.com/services/T000/B000/XXXX"},
			{Key: "channel", Label: "Channel override", Help: "Optional #channel or @user override"},
			{Key: "username", Label: "Bot username", Help: "Optional display name for the message"},
		},
	}
}

func (slackNotifier) Validate(config map[string]string) error {
	u := config["webhook_url"]
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return fmt.Errorf("webhook_url must be an http(s) URL")
	}
	return nil
}

func (slackNotifier) Send(ctx context.Context, config map[string]string, msg Message) error {
	text := msg.Body
	if msg.Subject != "" {
		text = "*" + msg.Subject + "*\n" + msg.Body
	}
	payload := map[string]any{"text": text}
	if c := config["channel"]; c != "" {
		payload["channel"] = c
	}
	if u := config["username"]; u != "" {
		payload["username"] = u
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config["webhook_url"], bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := webhookClient.Do(req)
	if err != nil {
		// Scrub the URL: a *url.Error embeds the secret webhook_url/token.
		return scrubURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned %d", resp.StatusCode)
	}
	return nil
}
