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

// actionOutcome is what an action produced, for callers that follow up on it. Only
// run_pipeline currently yields anything: the id of the run it started, which the GitHub App
// adapter turns into a check run on the commit that triggered it.
type actionOutcome struct {
	Kind  string
	RunID string
}

// runActions executes a matched trigger's actions in order, stopping at the first failure so
// the dispatch is retried as a unit. The outcomes of the actions that did run are returned
// even on failure — a later action failing does not un-start a pipeline an earlier one
// launched, and the caller still needs to track it.
func runActions(ctx context.Context, t Trigger, e Event) ([]actionOutcome, error) {
	evMap, err := e.asMap()
	if err != nil {
		return nil, err
	}
	var outs []actionOutcome
	for i, a := range t.Actions {
		out, err := runAction(ctx, t, a, e, evMap)
		countActionRun(ctx, a.Kind, err == nil)
		if out != nil {
			outs = append(outs, *out)
		}
		if err != nil {
			return outs, fmt.Errorf("action[%d] %s: %w", i, a.Kind, err)
		}
	}
	return outs, nil
}

func runAction(ctx context.Context, t Trigger, a Action, e Event, evMap map[string]any) (*actionOutcome, error) {
	switch a.Kind {
	case "run_pipeline":
		runID, err := actRunPipeline(ctx, a, e, evMap)
		if runID == "" {
			return nil, err
		}
		return &actionOutcome{Kind: a.Kind, RunID: runID}, err
	case "webhook_out":
		return nil, actWebhookOut(ctx, t, a, e, evMap)
	case "create_ticket":
		return nil, actInternalPost(ctx, ticketsURL+"/internal/tickets", a, e, evMap)
	case "notify":
		return nil, actInternalPost(ctx, envOrDefault("NOTIFICATIONS_URL", "http://localhost:8088")+"/internal/notify", a, e, evMap)
	case "enqueue_outpost_command":
		return nil, actInternalPost(ctx, envOrDefault("OUTPOST_GATEWAY_URL", "http://localhost:8092")+"/internal/commands", a, e, evMap)
	default:
		return nil, fmt.Errorf("unknown action kind %q", a.Kind)
	}
}

// ── run_pipeline (folds the former hooks dispatchWorkflow) ───────────────────────────────
//
// Returns the id of the run workflows started, so a caller that needs to track the run
// (the GitHub App check-run reporter) can. An empty id with a nil error means workflows
// accepted the dispatch but its response carried no run_id — the run exists, we just cannot
// follow it, which is not worth failing the action over.
func actRunPipeline(ctx context.Context, a Action, e Event, evMap map[string]any) (string, error) {
	pipelineID, _ := a.Config["pipeline_id"].(string)
	if pipelineID == "" {
		return "", fmt.Errorf("run_pipeline: pipeline_id is required")
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
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hooks-Token", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Hooks-Timestamp", ts)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s -> %d: %s", url, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(respBody, &out) // an unparseable body is not a dispatch failure
	return out.RunID, nil
}

// ── webhook_out — POST the (templated) event to a customer URL, signed ───────────────────
func actWebhookOut(ctx context.Context, t Trigger, a Action, e Event, evMap map[string]any) error {
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
	if ref, ok := a.Config["sign_secret"].(string); ok && ref != "" {
		key, err := resolveSignSecret(ctx, t, ref)
		if err != nil {
			return fmt.Errorf("webhook_out: %w", err)
		}
		mac := hmac.New(sha256.New, []byte(key))
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

// resolveSignSecret turns a trigger's sign_secret config into the signing key. The platform
// convention is the `secret:<name>` indirection: the value lives in gatekeeper under the
// trigger owner's scope, so the trigger document never holds a key and a config string is
// never used as one. Anything else is a configuration error, not a fallback — the action
// fails rather than sign with something the recipient cannot have.
func resolveSignSecret(ctx context.Context, t Trigger, ref string) (string, error) {
	scheme, name, ok := strings.Cut(ref, ":")
	if !ok || scheme != "secret" || name == "" {
		return "", fmt.Errorf("sign_secret must be a %q reference", "secret:<name>")
	}
	value, err := lookupScopedSecret(ctx, t.OrgID, t.CreatedBy, name)
	if err != nil {
		return "", fmt.Errorf("resolve sign_secret %q: %w", ref, err)
	}
	return value, nil
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
