package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// runActions executes a matched trigger's actions in order, stopping at the first failure so
// the dispatch is retried as a unit.
func runActions(ctx context.Context, t Trigger, e Event) error {
	evMap, err := e.asMap()
	if err != nil {
		return err
	}
	for i, a := range t.Actions {
		if err := runAction(ctx, a, e, evMap); err != nil {
			return fmt.Errorf("action[%d] %s: %w", i, a.Kind, err)
		}
	}
	return nil
}

func runAction(ctx context.Context, a Action, e Event, evMap map[string]any) error {
	switch a.Kind {
	case "run_pipeline":
		return actRunPipeline(ctx, a, e, evMap)
	case "webhook_out":
		return actWebhookOut(ctx, a, e, evMap)
	case "create_ticket":
		return actInternalPost(ctx, ticketsURL+"/internal/tickets", a, e, evMap)
	case "notify":
		return actInternalPost(ctx, envOrDefault("NOTIFICATIONS_URL", "http://localhost:8088")+"/internal/notify", a, e, evMap)
	case "enqueue_outpost_command":
		return actInternalPost(ctx, envOrDefault("OUTPOST_GATEWAY_URL", "http://localhost:8092")+"/internal/commands", a, e, evMap)
	default:
		return fmt.Errorf("unknown action kind %q", a.Kind)
	}
}

// ── run_pipeline (folds the former hooks dispatchWorkflow) ───────────────────────────────
func actRunPipeline(ctx context.Context, a Action, e Event, evMap map[string]any) error {
	pipelineID, _ := a.Config["pipeline_id"].(string)
	if pipelineID == "" {
		return fmt.Errorf("run_pipeline: pipeline_id is required")
	}
	inputs := map[string]string{}
	if raw, ok := a.Config["inputs"].(map[string]any); ok {
		for k, v := range raw {
			inputs[k] = interpolate(fmt.Sprint(v), evMap)
		}
	}
	body, _ := json.Marshal(map[string]any{
		"triggered_by": e.Actor.UserID,
		"org_id":       e.Actor.OrgID,
		"inputs":       inputs,
	})
	// workflows verifies its internal dispatch with X-Hooks-Token = HMAC(sharedKey,
	// "hooks:<pipeline>:<triggered_by>:<ts>") — the scheme it has always used for hook→run
	// dispatch. events shares that key (EVENTS_TRIGGER_KEY == the workflows trigger key).
	ts := fmt.Sprint(time.Now().UTC().Unix())
	mac := hmac.New(sha256.New, []byte(eventsTriggerKey))
	fmt.Fprintf(mac, "hooks:%s:%s:%s", pipelineID, e.Actor.UserID, ts)
	url := workflowsURL + "/internal/pipelines/" + pipelineID + "/runs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hooks-Token", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Hooks-Timestamp", ts)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s -> %d: %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// ── webhook_out — POST the (templated) event to a customer URL, signed ───────────────────
func actWebhookOut(ctx context.Context, a Action, e Event, evMap map[string]any) error {
	url, _ := a.Config["url"].(string)
	if url == "" {
		return fmt.Errorf("webhook_out: url is required")
	}
	payload, _ := json.Marshal(e)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if secretName, ok := a.Config["sign_secret"].(string); ok && secretName != "" {
		mac := hmac.New(sha256.New, []byte(secretName))
		mac.Write(payload)
		req.Header.Set("X-Events-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook_out %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// actInternalPost sends a templated action body to an internal service endpoint (tickets,
// notifications, outpost-gateway), authenticated by the shared events HMAC.
func actInternalPost(ctx context.Context, url string, a Action, e Event, evMap map[string]any) error {
	body := map[string]any{"org_id": e.Actor.OrgID, "user_id": e.Actor.UserID, "event": e}
	for k, v := range a.Config {
		if s, ok := v.(string); ok {
			body[k] = interpolate(s, evMap)
		} else {
			body[k] = v
		}
	}
	b, _ := json.Marshal(body)
	return internalPost(ctx, url, b)
}

// internalPost POSTs a JSON body to a trusted internal endpoint, signed with the shared key.
func internalPost(ctx context.Context, url string, body []byte) error {
	ts := fmt.Sprint(time.Now().UTC().Unix())
	mac := hmac.New(sha256.New, []byte(eventsTriggerKey))
	fmt.Fprintf(mac, "dispatch:%s", ts)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Events-Token", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Events-Timestamp", ts)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s -> %d: %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// ── templating ───────────────────────────────────────────────────────────────────────────
var tmplRe = regexp.MustCompile(`\{\{\s*([\w.]+)\s*\}\}`)

// interpolate replaces {{ path }} references with the value at that dotted path in the event
// (e.g. "{{ data.ref }}" -> "dev"). Unknown paths render as empty, so a template never fails.
func interpolate(s string, evMap map[string]any) string {
	return tmplRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := tmplRe.FindStringSubmatch(m)
		if len(sub) < 2 {
			return ""
		}
		if v, ok := lookup(sub[1], evMap); ok {
			return toStr(v)
		}
		return ""
	})
}
