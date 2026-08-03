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
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
)

// GitHub App adapter — POST /hooks/github.
//
// Distinct from the other adapters in both directions: inbound it verifies the App's own
// webhook secret (not the deployment-wide one), and outbound it reports back. When a matched
// trigger starts a pipeline run, the App opens a **check run** on the pushed commit and keeps
// it updated until the run finishes, so the result appears on the PR in GitHub's UI.
//
// That reporting is why the adapter needs the dispatch outcome rather than just ingesting the
// event: the check run is keyed to the run id the trigger's run_pipeline action produced.

type githubApp struct {
	appID         int64
	privateKey    *rsa.PrivateKey
	webhookSecret string
	apiBase       string

	tokenMu    sync.Mutex
	tokenCache map[int64]instToken
}

// instToken is a cached installation access token. GitHub issues these per installation with a
// one-hour life; caching avoids a mint round-trip on every webhook.
type instToken struct {
	value     string
	expiresAt time.Time
}

// newGithubApp parses the PEM private key (PKCS1, then PKCS8) and builds the client.
func newGithubApp(appID int64, privateKeyPEM, webhookSecret string) (*githubApp, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block from GitHub App private key")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		parsed, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("failed to parse GitHub App private key (PKCS1: %v, PKCS8: %v)", err, err2)
		}
		var ok bool
		if key, ok = parsed.(*rsa.PrivateKey); !ok {
			return nil, fmt.Errorf("GitHub App private key is not an RSA key")
		}
	}
	return &githubApp{
		appID:         appID,
		privateKey:    key,
		webhookSecret: webhookSecret,
		apiBase:       githubAPIBase,
		tokenCache:    make(map[int64]instToken),
	}, nil
}

// b64url encodes as unpadded base64url, the JWT wire encoding.
func b64url(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

// makeJWT builds the short-lived RS256 assertion that authenticates as the App itself (as
// opposed to as an installation). iat is backdated a minute to tolerate clock skew against
// GitHub, which rejects a future-dated token outright.
func (a *githubApp) makeJWT() (string, error) {
	now := time.Now()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("marshal JWT header: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.appID, 10),
	})
	if err != nil {
		return "", fmt.Errorf("marshal JWT payload: %w", err)
	}
	unsigned := b64url(header) + "." + b64url(payload)
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
	a.setGitHubHeaders(req)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("installation token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
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
	// Expire ours before GitHub expires theirs, so a token is never used in the moments after
	// it goes stale.
	expiresAt := time.Now().Add(55 * time.Minute)
	if t, err := time.Parse(time.RFC3339, result.ExpiresAt); err == nil {
		expiresAt = t.Add(-5 * time.Minute)
	}
	a.tokenCache[installationID] = instToken{value: result.Token, expiresAt: expiresAt}
	return result.Token, nil
}

func (a *githubApp) setGitHubHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// createCheckRun opens an in-progress check run on the commit and returns its id.
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
	req.Header.Set("Content-Type", "application/json")
	a.setGitHubHeaders(req)

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("create check run request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
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

// completeCheckRun closes a check run with a conclusion. Errors are logged rather than
// returned: every caller is a background goroutine with nowhere to return them to.
func (a *githubApp) completeCheckRun(ctx context.Context, token, ownerRepo string, checkRunID int64, conclusion string) {
	body, err := json.Marshal(map[string]any{
		"status":       "completed",
		"conclusion":   conclusion,
		"completed_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		slog.ErrorContext(ctx, "github_app: marshal complete check run", "check_run_id", checkRunID, "error", err)
		return
	}
	url := fmt.Sprintf("%s/repos/%s/check-runs/%d", a.apiBase, ownerRepo, checkRunID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		slog.ErrorContext(ctx, "github_app: build complete check run", "check_run_id", checkRunID, "error", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	a.setGitHubHeaders(req)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "github_app: complete check run", "check_run_id", checkRunID, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		slog.ErrorContext(ctx, "github_app: complete check run: unexpected status",
			"check_run_id", checkRunID, "status", resp.StatusCode, "body", strings.TrimSpace(string(b)))
	}
}

// verifySignature checks the App's webhook HMAC. This is the App's own secret, not the
// deployment-wide EVENTS_WEBHOOK_SECRET: GitHub generates it per App, and it is the only thing
// distinguishing a real delivery from anyone who knows the endpoint.
func (a *githubApp) verifySignature(body []byte, sigHeader string) bool {
	if a.webhookSecret == "" || sigHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(a.webhookSecret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sigHeader))
}

// pollRunStatus reads a run's current status from the workflows internal API. Returns "" on
// any error, which the caller treats as "not terminal yet" and retries.
func pollRunStatus(ctx context.Context, runID string) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(eventsTriggerKey))
	fmt.Fprintf(mac, "hooks-poll:%s:%s", runID, ts)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, workflowsURL+"/internal/runs/"+runID, nil)
	if err != nil {
		slog.ErrorContext(ctx, "github_app: poll run: build request", "run_id", runID, "error", err)
		return ""
	}
	req.Header.Set("X-Hooks-Token", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Hooks-Timestamp", ts)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "github_app: poll run: request", "run_id", runID, "error", err)
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

// checkRunWatchTimeout bounds how long a check run is left open waiting for its pipeline. A run
// that outlives it is marked timed_out rather than left in-progress forever on the PR.
const checkRunWatchTimeout = 2 * time.Hour

// watchAndCompleteCheckRun polls the run until terminal, then closes the check run with the
// matching conclusion.
func (a *githubApp) watchAndCompleteCheckRun(ctx context.Context, runID, ownerRepo string, installationID, checkRunID int64) {
	deadline := time.Now().Add(checkRunWatchTimeout)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			conclusion := ""
			if time.Now().After(deadline) {
				conclusion = "timed_out"
				slog.WarnContext(ctx, "github_app: watch run timed out", "run_id", runID, "check_run_id", checkRunID)
			} else {
				switch pollRunStatus(ctx, runID) {
				case "completed":
					conclusion = "success"
				case "failed":
					conclusion = "failure"
				case "cancelled":
					conclusion = "cancelled"
				}
			}
			if conclusion == "" {
				continue // still running
			}
			tok, err := a.installationToken(ctx, installationID)
			if err != nil {
				slog.ErrorContext(ctx, "github_app: watch run: installation token", "run_id", runID, "error", err)
				return
			}
			a.completeCheckRun(ctx, tok, ownerRepo, checkRunID, conclusion)
			slog.InfoContext(ctx, "github_app: check run completed",
				"run_id", runID, "check_run_id", checkRunID, "conclusion", conclusion)
			return
		}
	}
}

// normalizeGitHubEvent converts a GitHub webhook body into an envelope, also returning the
// installation id (needed to mint a token for the check run). ok=false for an event type we do
// not translate.
func normalizeGitHubEvent(eventType string, body []byte) (Event, int64, bool) {
	var installationID int64
	var envelope struct {
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		installationID = envelope.Installation.ID
	}

	switch eventType {
	case "push":
		var p struct {
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
		if err := json.Unmarshal(body, &p); err != nil || p.Repository.FullName == "" {
			return Event{}, 0, false
		}
		return Event{
			Type:    "repo.push",
			Source:  "github",
			Subject: p.Repository.FullName,
			Data:    gitPushData(stripRef(p.Ref), p.HeadCommit.ID, p.Pusher.Name, p.HeadCommit.Message),
		}, installationID, true

	case "pull_request":
		var p struct {
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
		if err := json.Unmarshal(body, &p); err != nil || p.Repository.FullName == "" {
			return Event{}, 0, false
		}
		return Event{
			Type:    "repo.pull_request." + p.Action,
			Source:  "github",
			Subject: p.Repository.FullName,
			Data:    gitPushData(p.PullRequest.Head.Ref, p.PullRequest.Head.SHA, p.PullRequest.User.Login, p.PullRequest.Title),
		}, installationID, true

	default:
		return Event{}, 0, false
	}
}

// checkRunObserver returns the dispatch observer that turns each pipeline run a trigger
// started into a check run on the commit, then watches it to completion.
//
// It detaches from the request context deliberately: the watch outlives the webhook response
// by as long as the pipeline takes to run.
func (a *githubApp) checkRunObserver(ownerRepo, headSHA string, installationID int64) dispatchObserver {
	return func(ctx context.Context, t Trigger, _ Event, outs []actionOutcome) {
		if installationID == 0 || headSHA == "" {
			return // nothing to report against
		}
		for _, out := range outs {
			if out.Kind != "run_pipeline" || out.RunID == "" {
				continue
			}
			go func(runID, triggerName string) {
				bg := detach(ctx)
				tok, err := a.installationToken(bg, installationID)
				if err != nil {
					slog.ErrorContext(bg, "github_app: installation token for check run",
						"run_id", runID, "installation_id", installationID, "error", err)
					return
				}
				checkRunID, err := a.createCheckRun(bg, tok, ownerRepo, headSHA, triggerName, runID)
				if err != nil {
					slog.ErrorContext(bg, "github_app: create check run", "run_id", runID, "error", err)
					return
				}
				slog.InfoContext(bg, "github_app: check run created", "run_id", runID, "check_run_id", checkRunID)
				a.watchAndCompleteCheckRun(bg, runID, ownerRepo, installationID, checkRunID)
			}(out.RunID, t.Name)
		}
	}
}

// handleGitHubWebhook returns the handler for POST /hooks/github. It is only registered when a
// GitHub App is configured — without one there is no secret to verify deliveries against.
func handleGitHubWebhook(app *githubApp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleGitHubWebhook")
		defer span.End()

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if !app.verifySignature(body, r.Header.Get("X-Hub-Signature-256")) {
			slog.WarnContext(ctx, "github_app: signature verification failed")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		switch eventType := r.Header.Get("X-GitHub-Event"); eventType {
		case "ping":
			w.WriteHeader(http.StatusOK)
			return
		case "installation", "installation_repositories":
			slog.InfoContext(ctx, "github_app: installation event", "event", eventType)
			w.WriteHeader(http.StatusOK)
			return
		default:
			e, installationID, ok := normalizeGitHubEvent(eventType, body)
			if !ok {
				w.WriteHeader(http.StatusOK) // unsupported type: acknowledge and ignore
				return
			}
			e.Actor = tenantFromQuery(r)
			headSHA, _ := e.Data["commit"].(string)
			obs := app.checkRunObserver(e.Subject, headSHA, installationID)
			if !ingestAdapterEvent(ctx, w, e, obs) {
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": e.ID})
		}
	}
}
