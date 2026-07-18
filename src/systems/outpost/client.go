package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// gatewayClient is the outpost's only outbound dependency: it dials the
// outpost-gateway over HTTPS (long-poll commands, POST events, heartbeat). No
// inbound connectivity to the outpost is required.
type gatewayClient struct {
	baseURL    string
	stateFile  string
	http       *http.Client
	outpostID  string
	outpostKey string
}

type outpostState struct {
	OutpostID  string `json:"outpost_id"`
	OutpostKey string `json:"outpost_key"`
}

func newGatewayClient(baseURL, stateFile string) *gatewayClient {
	return &gatewayClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		stateFile: stateFile,
		http:      buildHTTPClient(),
	}
}

// buildHTTPClient builds the outbound client for the gateway connection — the
// outpost's only trust boundary, and the channel that authorizes destructive
// in-cluster actions. It honours OUTPOST_CA_FILE so an operator can pin a
// private CA for the control plane instead of relying on the public trust store.
func buildHTTPClient() *http.Client {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile := os.Getenv("OUTPOST_CA_FILE"); caFile != "" {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			slog.Error("outpost: failed to read OUTPOST_CA_FILE", "path", caFile, "error", err)
			os.Exit(1)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			slog.Error("outpost: OUTPOST_CA_FILE contains no valid PEM certificates", "path", caFile)
			os.Exit(1)
		}
		tlsCfg.RootCAs = pool
	}
	// Wrap the transport so every gateway call becomes a client span and carries
	// W3C trace context to the gateway (linking the two sides of the connection).
	// otelhttp uses the global no-op provider until telemetry.Setup runs, so this
	// is free when OpenTelemetry is disabled.
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: otelhttp.NewTransport(&http.Transport{TLSClientConfig: tlsCfg}),
	}
}

// ensureEnrolled obtains credentials: explicit env, then persisted state, then a
// fresh enrollment with the single-use token (persisted for restarts).
func (g *gatewayClient) ensureEnrolled(ctx context.Context, id, key, enrollmentToken string) error {
	if id != "" && key != "" {
		g.outpostID, g.outpostKey = id, key
		return nil
	}
	if st, ok := g.loadState(); ok {
		g.outpostID, g.outpostKey = st.OutpostID, st.OutpostKey
		slog.Info("outpost: using persisted credentials", "outpost_id", g.outpostID)
		return nil
	}
	if enrollmentToken == "" {
		return fmt.Errorf("no credentials: set OUTPOST_ID/OUTPOST_KEY or ENROLLMENT_TOKEN")
	}
	return g.enroll(ctx, enrollmentToken)
}

func (g *gatewayClient) enroll(ctx context.Context, enrollmentToken string) error {
	body, _ := json.Marshal(map[string]string{"enrollment_token": enrollmentToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/outpost/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("enroll request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	slog.Debug("outpost: enroll response", "status", resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("enroll failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		OutpostID  string `json:"outpost_id"`
		OutpostKey string `json:"outpost_key"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("enroll: parse response: %w", err)
	}
	g.outpostID, g.outpostKey = out.OutpostID, out.OutpostKey
	g.saveState(outpostState{OutpostID: out.OutpostID, OutpostKey: out.OutpostKey})
	slog.Info("outpost: enrolled", "outpost_id", g.outpostID)
	return nil
}

// pollCommands holds open server-side up to ~30s and returns claimed commands.
func (g *gatewayClient) pollCommands(ctx context.Context) ([]Command, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/outpost/commands", nil)
	if err != nil {
		return nil, err
	}
	g.authHeaders(req)
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("unauthorized — credentials rejected")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("poll commands (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var cmds []Command
	if err := json.Unmarshal(raw, &cmds); err != nil {
		return nil, fmt.Errorf("poll commands: parse: %w", err)
	}
	slog.Debug("outpost: poll commands response", "status", resp.StatusCode, "count", len(cmds))
	return cmds, nil
}

// ackCommand marks a command terminal. status is CmdDone-equivalent ("done") on
// success or "failed" with a detail, so an enqueue-and-wait caller can gate on the
// real outcome rather than mere delivery. An empty status is treated as "done" by
// the gateway.
func (g *gatewayClient) ackCommand(ctx context.Context, id, status, errDetail string) error {
	body, _ := json.Marshal(map[string]string{"status": status, "error": errDetail})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/outpost/commands/"+id+"/ack", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	g.authHeaders(req)
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// A swallowed non-2xx (e.g. 401 on a rotated key, or a 5xx) must surface: an
	// ack the gateway never recorded leaves the command to be re-delivered, and
	// the caller needs to know so it doesn't treat the command as cleanly done.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ack command (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	slog.Debug("outpost: ack response", "command_id", id, "status", resp.StatusCode)
	return nil
}

// postEvent posts a single event. The caller MUST set ev.EventID before calling
// (postEventWithRetry does this once per logical event) so retries reuse one id
// and the gateway's at-least-once dedupe collapses them.
func (g *gatewayClient) postEvent(ctx context.Context, ev Event) error {
	body, _ := json.Marshal(ev)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/outpost/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	g.authHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("post event (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	// Log only safe metadata — never ev.Payload, which can carry cluster
	// resource/namespace names that land in the customer's logging stack.
	slog.Debug("outpost: post event response", "type", ev.Type, "integration", ev.Integration, "status", resp.StatusCode)
	return nil
}

func (g *gatewayClient) heartbeat(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/outpost/heartbeat", nil)
	if err != nil {
		return err
	}
	g.authHeaders(req)
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// Surface non-2xx so a revoked/expired key (401) doesn't masquerade as a
	// healthy heartbeat.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("heartbeat (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (g *gatewayClient) authHeaders(req *http.Request) {
	req.Header.Set("X-Outpost-ID", g.outpostID)
	req.Header.Set("Authorization", "Bearer "+g.outpostKey)
}

func (g *gatewayClient) loadState() (outpostState, bool) {
	if g.stateFile == "" {
		return outpostState{}, false
	}
	data, err := os.ReadFile(g.stateFile)
	if err != nil {
		return outpostState{}, false
	}
	var st outpostState
	if err := json.Unmarshal(data, &st); err != nil || st.OutpostID == "" || st.OutpostKey == "" {
		return outpostState{}, false
	}
	return st, true
}

func (g *gatewayClient) saveState(st outpostState) {
	if g.stateFile == "" {
		slog.Warn("outpost: OUTPOST_STATE_FILE unset — credentials not persisted; a restart cannot re-use the single-use enrollment token")
		return
	}
	if dir := filepath.Dir(g.stateFile); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(g.stateFile, data, 0o600); err != nil {
		slog.Error("outpost: failed to persist credentials", "error", err)
	}
}

func randID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
