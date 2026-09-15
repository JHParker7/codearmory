package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// notifyBody is what the events service posts to /internal/notify: the tenant scope plus
// the full event envelope (see events/actions.go actInternalPost).
type notifyBody struct {
	OrgID  string `json:"org_id"`
	UserID string `json:"user_id"`
	Event  Event  `json:"event"`
}

const dispatchWindow = 30 * time.Second

// verifyDispatchToken mirrors the events service's internalPost signing: HMAC over the
// literal "dispatch:<ts>" with the shared EVENTS_TRIGGER_KEY, within a 30s clock window.
func verifyDispatchToken(token, ts string) bool {
	if eventsTriggerKey == "" || token == "" || ts == "" {
		return false
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if skew := time.Since(time.Unix(secs, 0)); skew > dispatchWindow || skew < -dispatchWindow {
		return false
	}
	mac := hmac.New(sha256.New, []byte(eventsTriggerKey))
	fmt.Fprintf(mac, "dispatch:%s", ts)
	return hmac.Equal([]byte(token), []byte(hex.EncodeToString(mac.Sum(nil))))
}

func handleInternalNotify(w http.ResponseWriter, r *http.Request) {
	if !verifyDispatchToken(r.Header.Get("X-Events-Token"), r.Header.Get("X-Events-Timestamp")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body notifyBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	// Find the enabled channels that want this event: type in Events (or "*"), scoped to
	// the event's tenant (same org, or the same user when org-less).
	q := connect().WithContext(ctx).Where("enabled = ?", true)
	if body.OrgID != "" {
		q = q.Where("org_id = ?", body.OrgID)
	} else if body.UserID != "" {
		q = q.Where("created_by = ?", body.UserID)
	}
	var channels []Channel
	if err := q.Find(&channels).Error; err != nil {
		slog.ErrorContext(ctx, "notify: list channels", "error", err)
		http.Error(w, "failed to load channels", http.StatusInternalServerError)
		return
	}

	msg := render(body.Event)
	delivered := 0
	for i := range channels {
		c := &channels[i]
		if !wantsEvent(c, body.Event.Type) {
			continue
		}
		status, errStr := "delivered", ""
		if err := deliver(ctx, c, msg, body.Event); err != nil {
			status, errStr = "failed", err.Error()
			slog.WarnContext(ctx, "notify: delivery failed", "channel", c.ChannelID, "provider", c.Provider, "error", errStr)
		} else {
			delivered++
		}
		_ = connect().WithContext(ctx).Create(&Delivery{
			DeliveryID: uuid.NewString(), ChannelID: c.ChannelID, EventType: body.Event.Type,
			EventID: body.Event.ID, Status: status, Error: errStr, CreatedAt: time.Now().UTC(),
		}).Error
	}
	// Always 200: a failed downstream webhook must not make events retry the whole event.
	writeJSON(w, http.StatusOK, map[string]int{"matched": len(channels), "delivered": delivered})
}

// wantsEvent reports whether a channel subscribes to this event type ("*" matches all;
// a trailing "*" — e.g. "repo.*" — matches by prefix).
func wantsEvent(c *Channel, eventType string) bool {
	for _, e := range c.Events {
		if e == "*" || e == eventType {
			return true
		}
		if len(e) > 1 && e[len(e)-1] == '*' && len(eventType) >= len(e)-1 && eventType[:len(e)-1] == e[:len(e)-1] {
			return true
		}
	}
	return false
}
