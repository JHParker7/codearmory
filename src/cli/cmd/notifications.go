package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

var notificationsCmd = &cobra.Command{
	Use:     "notifications",
	Aliases: []string{"notify"},
	Short:   "Manage notification channels and send notifications",
}

var notificationsChannelsCmd = &cobra.Command{
	Use:   "channels",
	Short: "Manage notification channels (Slack, email, webhook, …)",
}

// notification command flags.
var (
	channelName    string
	channelType    string
	channelConfig  []string
	channelEnabled bool
	notifySubject  string
	notifyBody     string
	notifyChannels []string
)

// parseConfigPairs turns repeated --config key=value flags into a map.
func parseConfigPairs(pairs []string) (map[string]string, error) {
	config := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--config must be key=value, got %q", p)
		}
		config[k] = v
	}
	return config, nil
}

func init() {
	// ── armory notifications providers ────────────────────────────────────────
	notificationsCmd.AddCommand(&cobra.Command{
		Use:   "providers",
		Short: "List available integration providers and their config fields",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/notifications/providers", nil) },
	})

	// ── armory notifications channels list ────────────────────────────────────
	notificationsChannelsCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List notification channels",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/notifications/channels", nil) },
	})

	// ── armory notifications channels get <id> ────────────────────────────────
	notificationsChannelsCmd.AddCommand(&cobra.Command{
		Use:   "get <channel-id>",
		Short: "Get a notification channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/notifications/channels/"+args[0], nil)
		},
	})

	// ── armory notifications channels create ──────────────────────────────────
	createChannelCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a notification channel",
		Long: `Create a notification channel for a provider integration.

  armory notifications channels create --name "Team Slack" --type slack \
    --config webhook_url=https://hooks.slack.com/services/T/B/X
  armory notifications channels create --name "Ops email" --type email \
    --config host=smtp.example.com --config from=ops@example.com --config to=team@example.com`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if channelName == "" {
				return fmt.Errorf("--name is required")
			}
			if channelType == "" {
				return fmt.Errorf("--type is required (see: armory notifications providers)")
			}
			config, err := parseConfigPairs(channelConfig)
			if err != nil {
				return err
			}
			payload := map[string]any{
				"name":    channelName,
				"type":    channelType,
				"config":  config,
				"enabled": channelEnabled,
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/notifications/channels", body)
		},
	}
	createChannelCmd.Flags().StringVar(&channelName, "name", "", "channel name")
	createChannelCmd.Flags().StringVar(&channelType, "type", "", "provider type: slack, email, or webhook")
	createChannelCmd.Flags().StringArrayVar(&channelConfig, "config", nil, "provider config as key=value (repeatable)")
	createChannelCmd.Flags().BoolVar(&channelEnabled, "enabled", true, "whether the channel is enabled")
	notificationsChannelsCmd.AddCommand(createChannelCmd)

	// ── armory notifications channels delete <id> ─────────────────────────────
	notificationsChannelsCmd.AddCommand(&cobra.Command{
		Use:   "delete <channel-id>",
		Short: "Delete a notification channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/notifications/channels/"+args[0], nil)
		},
	})

	// ── armory notifications channels test <id> ───────────────────────────────
	testChannelCmd := &cobra.Command{
		Use:   "test <channel-id>",
		Short: "Send a test notification through a channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := map[string]any{}
			if notifySubject != "" {
				payload["subject"] = notifySubject
			}
			if notifyBody != "" {
				payload["body"] = notifyBody
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/notifications/channels/"+args[0]+"/test", body)
		},
	}
	testChannelCmd.Flags().StringVar(&notifySubject, "subject", "", "test message subject")
	testChannelCmd.Flags().StringVar(&notifyBody, "body", "", "test message body")
	notificationsChannelsCmd.AddCommand(testChannelCmd)

	// ── armory notifications send ─────────────────────────────────────────────
	sendCmd := &cobra.Command{
		Use:   "send",
		Short: "Send a notification to one or more channels",
		Long: `Send a notification. With no --channel, it is delivered to every enabled
channel you can access.

  armory notifications send --body "Deploy finished"
  armory notifications send --subject "Alert" --body "Disk full" --channel <id>`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if notifyBody == "" {
				return fmt.Errorf("--body is required")
			}
			payload := map[string]any{"body": notifyBody}
			if notifySubject != "" {
				payload["subject"] = notifySubject
			}
			if len(notifyChannels) > 0 {
				payload["channel_ids"] = notifyChannels
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/notifications/notify", body)
		},
	}
	sendCmd.Flags().StringVar(&notifySubject, "subject", "", "notification subject")
	sendCmd.Flags().StringVar(&notifyBody, "body", "", "notification body (required)")
	sendCmd.Flags().StringArrayVar(&notifyChannels, "channel", nil, "target channel id (repeatable; default: all enabled)")
	notificationsCmd.AddCommand(sendCmd)

	// ── armory notifications list ─────────────────────────────────────────────
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List recent notification delivery records",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/notifications/notifications"
			if status, _ := cmd.Flags().GetString("status"); status != "" {
				path += "?status=" + status
			}
			return apiCall("GET", path, nil)
		},
	}
	listCmd.Flags().String("status", "", "filter by status: pending, sent, or failed")
	notificationsCmd.AddCommand(listCmd)

	notificationsCmd.AddCommand(notificationsChannelsCmd)

	RegisterModule(Module{
		Name:    "notifications",
		Service: "notifications",
		Order:   25,
		Command: notificationsCmd,
		Screens: []HubScreen{{
			Title: "Notifications",
			Desc:  "Notification channels (Slack, email, webhook)",
			New:   func() tea.Model { return newNotificationsModel() },
		}},
	})
}
