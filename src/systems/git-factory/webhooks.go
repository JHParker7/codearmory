package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// Per-repo outbound webhooks.
//
// git_factory already emits events INTO the platform (events.go → the events service),
// which is what triggers pipelines. That is a closed loop: a consumer outside the
// platform — a chat notifier, someone else's CI, a deployment gate — has no way in
// without an operator wiring a trigger for them. A repo webhook is the escape hatch,
// and it is per-repo and user-configurable precisely because the internal event plane
// is neither.
//
// Delivery is best-effort and detached, exactly like the internal emit: a push that
// reached the disk has succeeded, and failing it afterwards because a listener was down
// would leave the client and the repo disagreeing. Failures are recorded on the row
// (LastStatus / LastError) rather than retried indefinitely, so an owner can SEE a
// broken endpoint instead of discovering it through absence.

type RepoWebhook struct {
	ID     string `gorm:"primaryKey" json:"id"`
	RepoID string `gorm:"index" json:"repo_id"`
	URL    string `json:"url"`
	// Secret keys the HMAC signature. Never returned by the API — see redacted().
	Secret string `json:"-"`
	// Events is a comma-separated list of event types to deliver, or "*" for all.
	Events    string    `json:"events"`
	Active    bool      `json:"active"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Delivery outcome of the most recent attempt, so a broken hook is visible.
	LastStatus    int        `json:"last_status,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	LastDeliverAt *time.Time `json:"last_delivered_at,omitempty"`
}

// wants reports whether this hook subscribes to an event type.
func (h RepoWebhook) wants(evType string) bool {
	if !h.Active {
		return false
	}
	for _, e := range strings.Split(h.Events, ",") {
		e = strings.TrimSpace(e)
		if e == "*" || e == evType {
			return true
		}
	}
	return false
}

// redacted is the API view. The secret is write-only: an owner who loses it rotates the
// hook rather than reading it back, which keeps a repo-read grant from being a way to
// harvest signing keys.
func (h RepoWebhook) redacted() map[string]any {
	return map[string]any{
		"id": h.ID, "repo_id": h.RepoID, "url": h.URL, "events": h.Events,
		"active": h.Active, "created_by": h.CreatedBy,
		"created_at": h.CreatedAt, "updated_at": h.UpdatedAt,
		"last_status": h.LastStatus, "last_error": h.LastError,
		"last_delivered_at": h.LastDeliverAt,
		"has_secret":        h.Secret != "",
	}
}

// validWebhookURL constrains where a hook may point.
//
// This is an SSRF boundary, not a formatting rule. A webhook is a user-supplied URL that
// a SERVER fetches, so without a scheme restriction a repo owner could aim it at an
// in-cluster address and use git_factory as a confused deputy against services that
// trust the network. Scheme is checked here; the address itself is checked at dial time
// in webhookDialer, because a hostname's resolution can change between the two.
func validWebhookURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("url must be http or https")
	}
	if u.Host == "" {
		return errors.New("url must have a host")
	}
	return nil
}

func handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListWebhooks")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listWebhook")
	if !ok {
		return
	}
	hooks, err := webhooksFor(ctx, re.ID)
	if err != nil {
		slog.ErrorContext(ctx, "list webhooks", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, h.redacted())
	}
	writeJSON(w, http.StatusOK, out)
}

func handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleCreateWebhook")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "setWebhook")
	if !ok {
		return
	}
	var req struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
		Events string `json:"events"`
		Active *bool  `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := validWebhookURL(req.URL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	events := strings.TrimSpace(req.Events)
	if events == "" {
		events = "*"
	}
	secret := strings.TrimSpace(req.Secret)
	if secret == "" {
		// Generate one rather than leaving the hook unsigned: an unsigned webhook gives
		// the receiver no way to tell a real delivery from anyone who learned the URL.
		// Returned ONCE, in this response, and never readable again.
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			slog.ErrorContext(ctx, "webhook secret generation", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		secret = hex.EncodeToString(b)
	}
	h := RepoWebhook{
		ID: uuid.New().String(), RepoID: re.ID, URL: strings.TrimSpace(req.URL),
		Secret: secret, Events: events, Active: req.Active == nil || *req.Active,
		CreatedBy: userID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().WithContext(ctx).Create(&h).Error; err != nil {
		slog.ErrorContext(ctx, "create webhook", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "repo webhook created", "Repo_id", re.ID, "hook_id", h.ID, "url", h.URL)
	out := h.redacted()
	out["secret"] = secret // the only time it is ever returned
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusCreated, out)
}

func handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleDeleteWebhook")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "setWebhook")
	if !ok {
		return
	}
	res := connect().WithContext(ctx).
		Where("id = ? AND repo_id = ?", r.PathValue("hook_id"), re.ID).Delete(&RepoWebhook{})
	if res.Error != nil {
		slog.ErrorContext(ctx, "delete webhook", "Repo_id", re.ID, "error", res.Error)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if res.RowsAffected == 0 {
		http.Error(w, "webhook not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func webhooksFor(ctx context.Context, repoID string) ([]RepoWebhook, error) {
	var hooks []RepoWebhook
	err := connectRead().WithContext(ctx).
		Where("repo_id = ?", repoID).Order("created_at").Find(&hooks).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return hooks, err
}

// webhookSignature is the header a receiver verifies: hex HMAC-SHA256 over the exact
// body bytes, in the same "sha256=" shape GitHub uses so existing receiver code works
// unchanged.
func webhookSignature(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// deliverWebhooks fans an event out to every subscribed hook on the repo. Detached from
// the caller's request; never blocks the operation that produced the event.
func deliverWebhooks(ctx context.Context, repoID, evType string, payload map[string]any) {
	// Hooks live in the database; with no handle open there are none to deliver to, and
	// asking would open one (and exit the process if it could not). See dbReady.
	if !dbReady() {
		return
	}
	hooks, err := webhooksFor(ctx, repoID)
	if err != nil || len(hooks) == 0 {
		return
	}
	body, err := json.Marshal(map[string]any{
		"event": evType,
		"data":  payload,
		// Sent so a receiver can reject a replayed delivery; it is inside the signed
		// body rather than a separate header, so it cannot be altered independently.
		"delivered_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	for _, h := range hooks {
		if !h.wants(evType) {
			continue
		}
		go deliverOne(ctx, h, evType, body)
	}
}

// webhookTimeout bounds a single delivery. A hook pointing at something that accepts
// the connection and then stalls must not hold a goroutine open indefinitely.
const webhookTimeout = 10 * time.Second

// webhookPrivateCIDRs are the internal ranges Go's net.IP predicates do not already
// cover. Same list the egress proxy blocks, for the same reason.
var webhookPrivateCIDRs = func() []*net.IPNet {
	var out []*net.IPNet
	for _, s := range []string{
		"100.64.0.0/10", // CGNAT
		"0.0.0.0/8",     // "this network"
		"192.0.0.0/24",  // IETF protocol assignments
		"198.18.0.0/15", // benchmarking
	} {
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// isInternalIP reports whether ip is somewhere a webhook must never reach.
func isInternalIP(ip net.IP) bool {
	// An IPv4-mapped IPv6 address must be judged as the IPv4 address it is, or
	// ::ffff:127.0.0.1 walks straight past the loopback test below.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range webhookPrivateCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// webhookClient delivers hooks. Its dialer refuses internal addresses.
//
// The URL is supplied by a repo owner and fetched by the SERVER, which is the classic
// confused-deputy setup: without this, a hook aimed at http://gatekeeper:8081 or at the
// cloud metadata endpoint would be a request made from inside the cluster, by a service
// that other services trust. The check is at DIAL time and against the address actually
// being connected to, which is what makes it hold against DNS rebinding — a name that
// resolved publicly when the hook was created can resolve to 127.0.0.1 at delivery.
var webhookClient = &http.Client{
	Timeout: webhookTimeout,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var d net.Dialer
			lastErr := errors.New("no permitted address for " + host)
			for _, ip := range ips {
				if isInternalIP(ip.IP) {
					lastErr = errors.New("webhook to internal address " + ip.IP.String() + " blocked")
					continue
				}
				// Dial the exact IP that was just checked, not the name — resolving
				// again here would reopen the rebinding window this closes.
				conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if derr == nil {
					return conn, nil
				}
				lastErr = derr
			}
			return nil, lastErr
		},
	},
}

func deliverOne(ctx context.Context, h RepoWebhook, evType string, body []byte) {
	ctx, span := otel.Tracer(serviceName).Start(
		trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx)),
		"deliverWebhook")
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		recordDelivery(ctx, h.ID, 0, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codearmory-git-factory")
	req.Header.Set("X-CodeArmory-Event", evType)
	req.Header.Set("X-CodeArmory-Delivery", uuid.New().String())
	if h.Secret != "" {
		req.Header.Set("X-CodeArmory-Signature-256", webhookSignature(h.Secret, body))
	}

	resp, err := webhookClient.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "webhook delivery failed", "hook_id", h.ID, "error", err)
		recordDelivery(ctx, h.ID, 0, err.Error())
		return
	}
	defer resp.Body.Close()
	msg := ""
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg = "endpoint returned " + resp.Status
	}
	recordDelivery(ctx, h.ID, resp.StatusCode, msg)
}

// recordDelivery stamps the outcome on the hook so a broken endpoint is visible in the
// API rather than only in logs. Uses WithoutCancel: the delivery context is already
// timing out by design, and losing the record is how a hook fails silently.
func recordDelivery(ctx context.Context, hookID string, status int, errMsg string) {
	now := time.Now().UTC()
	if err := connect().WithContext(context.WithoutCancel(ctx)).Model(&RepoWebhook{}).
		Where("id = ?", hookID).
		Updates(map[string]any{
			"last_status": status, "last_error": errMsg, "last_deliver_at": now,
		}).Error; err != nil {
		slog.WarnContext(ctx, "could not record webhook delivery", "hook_id", hookID, "error", err)
	}
}
