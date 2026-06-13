package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type githubApp struct {
	appID         int64
	privateKey    *rsa.PrivateKey
	webhookSecret string
	apiBase       string // "https://api.github.com" or GITHUB_API_URL override

	tokenMu    sync.Mutex
	tokenCache map[int64]instToken
}

type instToken struct {
	value     string
	expiresAt time.Time
}

// newGithubApp parses the PEM-encoded private key and initialises a githubApp.
// It tries PKCS1 first, then PKCS8.
func newGithubApp(appID int64, privateKeyPEM, webhookSecret string) (*githubApp, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block from GitHub App private key")
	}

	var privateKey *rsa.PrivateKey
	// Try PKCS1 first.
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		privateKey = key
	} else {
		// Try PKCS8.
		parsed, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("failed to parse GitHub App private key (tried PKCS1: %v, PKCS8: %v)", err, err2)
		}
		var ok bool
		privateKey, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("GitHub App private key is not an RSA key")
		}
	}

	apiBase := os.Getenv("GITHUB_API_URL")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}

	return &githubApp{
		appID:         appID,
		privateKey:    privateKey,
		webhookSecret: webhookSecret,
		apiBase:       apiBase,
		tokenCache:    make(map[int64]instToken),
	}, nil
}

// b64url encodes data as base64url with no padding.
func b64url(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

// makeJWT creates a signed RS256 JWT for GitHub App authentication.
func (a *githubApp) makeJWT() (string, error) {
	now := time.Now()

	headerJSON, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("marshal JWT header: %w", err)
	}
	payloadJSON, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.appID, 10),
	})
	if err != nil {
		return "", fmt.Errorf("marshal JWT payload: %w", err)
	}

	unsigned := b64url(headerJSON) + "." + b64url(payloadJSON)
	hash := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.privateKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	return unsigned + "." + b64url(sig), nil
}

// installationToken returns a cached or freshly minted installation access token.
func (a *githubApp) installationToken(ctx context.Context, installationID int64) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()

	// Return cached token if still valid (with 5 min safety margin).
	if tok, ok := a.tokenCache[installationID]; ok && time.Now().Before(tok.expiresAt) {
		return tok.value, nil
	}

	jwt, err := a.makeJWT()
	if err != nil {
		return "", fmt.Errorf("make JWT: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.apiBase, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", fmt.Errorf("build installation token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("installation token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("installation token: GitHub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Token == "" {
		return "", fmt.Errorf("installation token: failed to parse response")
	}

	expiresAt := time.Now().Add(55 * time.Minute) // default safety margin
	if t, err := time.Parse(time.RFC3339, result.ExpiresAt); err == nil {
		expiresAt = t.Add(-5 * time.Minute)
	}

	a.tokenCache[installationID] = instToken{value: result.Token, expiresAt: expiresAt}
	return result.Token, nil
}

// createCheckRun creates a GitHub check run in "in_progress" status and returns its ID.
func (a *githubApp) createCheckRun(ctx context.Context, token, ownerRepo, headSHA, name, externalID string) (int64, error) {
	body, err := json.Marshal(map[string]any{
		"name":        name,
		"head_sha":    headSHA,
		"status":      "in_progress",
		"started_at":  time.Now().UTC().Format(time.RFC3339),
		"external_id": externalID,
	})
	if err != nil {
		return 0, fmt.Errorf("marshal check run body: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/check-runs", a.apiBase, ownerRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build check run request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("create check run request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("create check run: GitHub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, fmt.Errorf("create check run: failed to parse response")
	}
	return result.ID, nil
}

// completeCheckRun updates a GitHub check run to "completed" with the given conclusion.
// Errors are logged but not returned — this is always called from a goroutine.
func (a *githubApp) completeCheckRun(ctx context.Context, token, ownerRepo string, checkRunID int64, conclusion string) {
	body, err := json.Marshal(map[string]any{
		"status":       "completed",
		"conclusion":   conclusion,
		"completed_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		slog.ErrorContext(ctx, "github_app: marshal complete check run body", "check_run_id", checkRunID, "error", err)
		return
	}

	url := fmt.Sprintf("%s/repos/%s/check-runs/%d", a.apiBase, ownerRepo, checkRunID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		slog.ErrorContext(ctx, "github_app: build complete check run request", "check_run_id", checkRunID, "error", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "github_app: complete check run request", "check_run_id", checkRunID, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		body := strings.TrimSpace(string(respBody))
		if len(body) > 512 {
			body = body[:512]
		}
		slog.ErrorContext(ctx, "github_app: complete check run: unexpected status",
			"check_run_id", checkRunID, "status", resp.StatusCode, "body", body)
	}
}

// verifySignature checks the GitHub webhook HMAC signature.
func (a *githubApp) verifySignature(body []byte, sigHeader string) bool {
	expected := "sha256=" + computeHMAC(a.webhookSecret, body)
	return hmac.Equal([]byte(expected), []byte(sigHeader))
}

// pollRunStatus fetches the current status of a workflow run from the internal
// workflows API. Returns the status string or "" on error.
func pollRunStatus(ctx context.Context, runID string) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(hooksTriggerKey))
	fmt.Fprintf(mac, "hooks-poll:%s:%s", runID, ts)
	token := hex.EncodeToString(mac.Sum(nil))

	url := workflowsURL + "/internal/runs/" + runID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		slog.ErrorContext(ctx, "github_app: poll run status: build request", "run_id", runID, "error", err)
		return ""
	}
	req.Header.Set("X-Hooks-Token", token)
	req.Header.Set("X-Hooks-Timestamp", ts)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "github_app: poll run status: request", "run_id", runID, "error", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}

	var result struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	return result.Status
}

// watchAndCompleteCheckRun polls a workflow run every 15 seconds until terminal,
// then updates the GitHub check run accordingly. Times out after 2 hours.
func (a *githubApp) watchAndCompleteCheckRun(ctx context.Context, runID, ownerRepo string, installationID, checkRunID int64) {
	bgCtx := context.Background()
	deadline := time.Now().Add(2 * time.Hour)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if time.Now().After(deadline) {
				slog.WarnContext(ctx, "github_app: watch run: timed out", "run_id", runID, "check_run_id", checkRunID)
				tok, err := a.installationToken(bgCtx, installationID)
				if err != nil {
					slog.ErrorContext(ctx, "github_app: watch run: get token for timeout", "error", err)
					return
				}
				a.completeCheckRun(bgCtx, tok, ownerRepo, checkRunID, "timed_out")
				return
			}

			status := pollRunStatus(bgCtx, runID)
			conclusion := ""
			switch status {
			case "completed":
				conclusion = "success"
			case "failed":
				conclusion = "failure"
			case "cancelled":
				conclusion = "cancelled"
			}

			if conclusion != "" {
				tok, err := a.installationToken(bgCtx, installationID)
				if err != nil {
					slog.ErrorContext(ctx, "github_app: watch run: get token", "run_id", runID, "error", err)
					return
				}
				a.completeCheckRun(bgCtx, tok, ownerRepo, checkRunID, conclusion)
				slog.InfoContext(ctx, "github_app: check run completed",
					"run_id", runID, "check_run_id", checkRunID, "conclusion", conclusion)
				return
			}

		case <-ctx.Done():
			return
		}
	}
}

// normalizeGitHubPayload converts a raw GitHub webhook body into a gitPayload.
// It handles "push" and "pull_request" event types. Returns (payload, installationID, ok).
func normalizeGitHubPayload(eventType string, rawBody []byte) (gitPayload, int64, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &raw); err != nil {
		return gitPayload{}, 0, false
	}

	// Extract installation ID (common to all events).
	var installationID int64
	if instRaw, ok := raw["installation"]; ok {
		var inst struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(instRaw, &inst); err == nil {
			installationID = inst.ID
		}
	}

	switch eventType {
	case "push":
		var push struct {
			Ref        string `json:"ref"`
			HeadCommit struct {
				ID      string `json:"id"`
				Message string `json:"message"`
			} `json:"head_commit"`
			Pusher struct {
				Name string `json:"name"`
			} `json:"pusher"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(rawBody, &push); err != nil {
			return gitPayload{}, 0, false
		}
		ref := push.Ref
		ref = strings.TrimPrefix(ref, "refs/heads/")
		ref = strings.TrimPrefix(ref, "refs/tags/")
		return gitPayload{
			Repo:    push.Repository.FullName,
			Event:   "push",
			Ref:     ref,
			Commit:  push.HeadCommit.ID,
			Pusher:  push.Pusher.Name,
			Message: push.HeadCommit.Message,
		}, installationID, true

	case "pull_request":
		var pr struct {
			Action      string `json:"action"`
			PullRequest struct {
				Head struct {
					Ref string `json:"ref"`
					SHA string `json:"sha"`
				} `json:"head"`
				User struct {
					Login string `json:"login"`
				} `json:"user"`
				Title string `json:"title"`
			} `json:"pull_request"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(rawBody, &pr); err != nil {
			return gitPayload{}, 0, false
		}
		return gitPayload{
			Repo:    pr.Repository.FullName,
			Event:   "pull_request." + pr.Action,
			Ref:     pr.PullRequest.Head.Ref,
			Commit:  pr.PullRequest.Head.SHA,
			Pusher:  pr.PullRequest.User.Login,
			Message: pr.PullRequest.Title,
		}, installationID, true

	default:
		return gitPayload{}, 0, false
	}
}

// handleGitHubWebhook returns an http.HandlerFunc that receives GitHub App webhooks.
func handleGitHubWebhook(app *githubApp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}

		// Verify App-level HMAC signature.
		if !app.verifySignature(rawBody, r.Header.Get("X-Hub-Signature-256")) {
			slog.WarnContext(ctx, "github_app: signature verification failed")
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		eventType := r.Header.Get("X-GitHub-Event")

		// Acknowledge ping events immediately.
		if eventType == "ping" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Log installation events and return.
		if eventType == "installation" || eventType == "installation_repositories" {
			slog.InfoContext(ctx, "github_app: installation event received", "event", eventType)
			w.WriteHeader(http.StatusOK)
			return
		}

		// Normalize the payload.
		payload, installationID, ok := normalizeGitHubPayload(eventType, rawBody)
		if !ok {
			// Unknown or unsupported event type — acknowledge and ignore.
			w.WriteHeader(http.StatusOK)
			return
		}

		meterHooksReceived.Add(ctx, 1, metric.WithAttributes(attribute.String("source", payload.Repo)))

		payloadMap := map[string]string{
			"repo":    payload.Repo,
			"event":   payload.Event,
			"ref":     payload.Ref,
			"commit":  payload.Commit,
			"pusher":  payload.Pusher,
			"message": payload.Message,
		}

		eventID := uuid.New().String()
		newEvent := HookEvent{
			EventID:   eventID,
			Source:    payload.Repo,
			EventType: payload.Event,
			Ref:       payload.Ref,
			Payload:   payloadMap,
			Status:    "received",
			CreatedAt: time.Now().UTC(),
		}
		if err := newEvent.Add(ctx); err != nil {
			slog.ErrorContext(ctx, "github_app: insert event", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		// App-level HMAC already verified above; skip per-rule HMAC checks.
		trigResults, successCount, failCount := matchAndDispatch(ctx, eventID, payload.Repo, payload.Event, payload.Ref, payloadMap, gitBaseInputs(payload), rawBody, "", true, "", "")

		// For successful dispatches with a run ID, create a GitHub check run and watch it.
		if installationID != 0 {
			for _, tr := range trigResults {
				if tr.Success && tr.Trigger.RunID != nil && *tr.Trigger.RunID != "" {
					runID := *tr.Trigger.RunID
					go func(runID, ruleName string) {
						bgCtx := context.Background()
						tok, err := app.installationToken(bgCtx, installationID)
						if err != nil {
							slog.Error("github_app: get installation token for check run",
								"run_id", runID, "installation_id", installationID, "error", err)
							return
						}
						checkRunID, err := app.createCheckRun(bgCtx, tok, payload.Repo, payload.Commit, ruleName, runID)
						if err != nil {
							slog.Error("github_app: create check run",
								"run_id", runID, "error", err)
							return
						}
						slog.Info("github_app: check run created",
							"run_id", runID, "check_run_id", checkRunID)
						app.watchAndCompleteCheckRun(bgCtx, runID, payload.Repo, installationID, checkRunID)
					}(runID, tr.RuleName)
				}
			}
		}

		// Collect triggers for response.
		triggers := make([]HookTrigger, 0, len(trigResults))
		for _, tr := range trigResults {
			triggers = append(triggers, tr.Trigger)
		}

		total := successCount + failCount
		eventStatus := "received"
		switch {
		case total == 0:
			eventStatus = "received"
		case failCount == 0:
			eventStatus = "triggered"
		case successCount == 0:
			eventStatus = "failed"
		default:
			eventStatus = "partial"
		}

		go func() { updateEventStatus(eventID, total, eventStatus) }()

		event := HookEvent{
			EventID:      eventID,
			Source:       payload.Repo,
			EventType:    payload.Event,
			Ref:          payload.Ref,
			Payload:      payloadMap,
			RulesMatched: total,
			Status:       eventStatus,
			Triggers:     triggers,
			CreatedAt:    time.Now().UTC(),
		}
		if event.Triggers == nil {
			event.Triggers = []HookTrigger{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(event) //nolint:errcheck
	}
}
