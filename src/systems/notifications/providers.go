package main

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
)

// Message is the provider-agnostic payload a Notifier delivers.
type Message struct {
	Subject string
	Body    string
}

// ConfigField describes one provider configuration setting, used both to
// validate a Channel's Config and to drive dynamic forms in the CLI/portal.
type ConfigField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
	Secret   bool   `json:"secret"`
	Help     string `json:"help,omitempty"`
}

// ProviderInfo is the public description of a provider plugin.
type ProviderInfo struct {
	Type   string        `json:"type"`
	Label  string        `json:"label"`
	Fields []ConfigField `json:"fields"`
}

// Notifier is the pluggable integration contract. Each delivery backend (Slack,
// email, generic webhook, …) implements it and registers itself from an init()
// via RegisterNotifier. Adding a new integration is a single self-contained file
// — no other code in the service needs to change.
type Notifier interface {
	// Info returns the provider type, label, and config schema.
	Info() ProviderInfo
	// Validate checks a Channel's config before it is persisted.
	Validate(config map[string]string) error
	// Send delivers msg using config. It must be safe to call concurrently.
	Send(ctx context.Context, config map[string]string, msg Message) error
}

// errUnknownChannelType is returned when a channel references a provider type
// that is not registered (e.g. after a provider was removed from a build).
var errUnknownChannelType = fmt.Errorf("unknown channel type")

// notifiers is the plugin registry, keyed by provider type. Populated from the
// init() of each provider_*.go file.
var notifiers = map[string]Notifier{}

// RegisterNotifier adds n to the registry. Call it from a provider's init().
// Panics on a duplicate type so collisions surface at startup, not at runtime.
func RegisterNotifier(n Notifier) {
	t := n.Info().Type
	if _, dup := notifiers[t]; dup {
		panic("notifications: duplicate provider type " + t)
	}
	notifiers[t] = n
}

// getNotifier returns the registered provider for a channel type.
func getNotifier(channelType string) (Notifier, bool) {
	n, ok := notifiers[channelType]
	return n, ok
}

// providerInfos returns the schema for every registered provider, sorted by
// type for stable output.
func providerInfos() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(notifiers))
	for _, n := range notifiers {
		out = append(out, n.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// validateConfig checks required fields and rejects unknown keys for a provider,
// then defers to the provider's own Validate. config is mutated to drop empty
// values so stored configs stay tidy.
func validateConfig(channelType string, config map[string]string) error {
	n, ok := getNotifier(channelType)
	if !ok {
		return fmt.Errorf("unknown channel type %q", channelType)
	}
	known := map[string]bool{}
	for _, f := range n.Info().Fields {
		known[f.Key] = true
		if f.Required && strings.TrimSpace(config[f.Key]) == "" {
			return fmt.Errorf("missing required field %q", f.Key)
		}
	}
	for k := range config {
		if !known[k] {
			return fmt.Errorf("unknown config field %q for %s", k, channelType)
		}
	}
	return n.Validate(config)
}

// redactConfig returns a copy of config with secret fields masked, for safe
// inclusion in API responses.
func redactConfig(channelType string, config map[string]string) map[string]string {
	out := make(map[string]string, len(config))
	maps.Copy(out, config)
	if n, ok := getNotifier(channelType); ok {
		for _, f := range n.Info().Fields {
			if f.Secret && out[f.Key] != "" {
				out[f.Key] = redacted
			}
		}
	}
	return out
}

// mergeConfig folds an incoming config onto the existing stored config, treating
// a secret field left as the redacted sentinel as "keep the stored value". This
// lets a client PUT back a previously-fetched (redacted) channel without wiping
// its secrets.
func mergeConfig(channelType string, existing, incoming map[string]string) map[string]string {
	merged := make(map[string]string, len(incoming))
	maps.Copy(merged, incoming)
	if n, ok := getNotifier(channelType); ok {
		for _, f := range n.Info().Fields {
			if f.Secret && merged[f.Key] == redacted {
				merged[f.Key] = existing[f.Key]
			}
		}
	}
	return merged
}
