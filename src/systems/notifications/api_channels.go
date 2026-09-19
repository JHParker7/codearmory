package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type channelRequest struct {
	Name     string            `json:"name"`
	Provider string            `json:"provider"`
	Config   map[string]string `json:"config"`
	Events   []string          `json:"events"`
	Project  string            `json:"project"`
	Enabled  *bool             `json:"enabled"`
}

// requiredConfigKey is the one config field a provider cannot deliver without.
func requiredConfigKey(provider string) string {
	switch provider {
	case "slack", "discord", "webhook":
		return "url"
	case "email":
		return "to"
	}
	return ""
}

func handleListChannels(w http.ResponseWriter, r *http.Request) {
	userID, orgID, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listChannel", "notifications/channels")
	if !ok {
		return
	}
	q := connect().WithContext(r.Context())
	// A caller sees the channels they own, or their org's. Same owner/tenant scoping the
	// other services use.
	if orgID != "" {
		q = q.Where("created_by = ? OR org_id = ?", userID, orgID)
	} else {
		q = q.Where("created_by = ?", userID)
	}
	if p := strings.TrimSpace(r.URL.Query().Get("project")); p != "" {
		q = q.Where("project = ?", p)
	}
	var out []Channel
	if err := q.Order("created_at desc").Find(&out).Error; err != nil {
		http.Error(w, "failed to list channels", http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []Channel{}
	}
	writeJSON(w, http.StatusOK, out)
}

func handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	userID, orgID, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "createChannel", "notifications/channels")
	if !ok {
		return
	}
	var req channelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if !validProvider(req.Provider) {
		http.Error(w, "provider must be one of slack, discord, webhook, email", http.StatusBadRequest)
		return
	}
	if key := requiredConfigKey(req.Provider); key != "" && strings.TrimSpace(req.Config[key]) == "" {
		http.Error(w, "config."+key+" is required for provider "+req.Provider, http.StatusBadRequest)
		return
	}
	if len(req.Events) == 0 {
		req.Events = []string{"*"}
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	now := time.Now().UTC()
	c := Channel{
		ChannelID: uuid.NewString(),
		Name:      req.Name,
		Provider:  req.Provider,
		Config:    req.Config,
		Events:    req.Events,
		Project:   req.Project,
		Enabled:   enabled,
		CreatedBy: userID,
		OrgID:     orgID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := connect().WithContext(r.Context()).Create(&c).Error; err != nil {
		http.Error(w, "failed to create channel", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// ownedChannel loads a channel and confirms the caller owns it (or its org does).
func ownedChannel(w http.ResponseWriter, r *http.Request, action string) (*Channel, bool) {
	userID, orgID, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, action, "notifications/channels/"+r.PathValue("id"))
	if !ok {
		return nil, false
	}
	var c Channel
	if err := connect().WithContext(r.Context()).Where("channel_id = ?", r.PathValue("id")).First(&c).Error; err != nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return nil, false
	}
	if c.CreatedBy != userID && !(orgID != "" && c.OrgID == orgID) {
		http.Error(w, "channel not found", http.StatusNotFound)
		return nil, false
	}
	return &c, true
}

func handleGetChannel(w http.ResponseWriter, r *http.Request) {
	c, ok := ownedChannel(w, r, "getChannel")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	c, ok := ownedChannel(w, r, "updateChannel")
	if !ok {
		return
	}
	var req channelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) != "" {
		c.Name = req.Name
	}
	if req.Config != nil {
		c.Config = req.Config
	}
	if req.Events != nil {
		c.Events = req.Events
	}
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	c.Project = req.Project
	c.UpdatedAt = time.Now().UTC()
	if err := connect().WithContext(r.Context()).Save(c).Error; err != nil {
		http.Error(w, "failed to update channel", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	c, ok := ownedChannel(w, r, "deleteChannel")
	if !ok {
		return
	}
	if err := connect().WithContext(r.Context()).Delete(&Channel{}, "channel_id = ?", c.ChannelID).Error; err != nil {
		http.Error(w, "failed to delete channel", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleTestChannel(w http.ResponseWriter, r *http.Request) {
	c, ok := ownedChannel(w, r, "testChannel")
	if !ok {
		return
	}
	msg := "✅ CodeArmory notifications test — channel \"" + c.Name + "\" (" + c.Provider + ") is wired up."
	if err := deliver(r.Context(), c, msg, Event{Type: "notifications.test", Source: "notifications"}); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"status": "failed", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "delivered"})
}
