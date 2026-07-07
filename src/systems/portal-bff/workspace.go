package main

import "sort"

// This file maps raw conductor/terraform state responses into the uniform
// workspaceView the SPA consumes. Conductor answers a state GET three ways — 204
// (empty), 423 (locked, with lock info), or 200 (the full tfstate); the three
// from* builders collapse those into one shape so the client never branches on
// HTTP status.

// terraformState mirrors the fields of a tfstate document the BFF summarizes.
type terraformState struct {
	TerraformVersion string              `json:"terraform_version"`
	Serial           int64               `json:"serial"`
	Lineage          string              `json:"lineage"`
	Outputs          map[string]any      `json:"outputs"`
	Resources        []terraformResource `json:"resources"`
}

type terraformResource struct {
	Type string `json:"type"`
}

// lockInfo is terraform's lock document (the conductor 423 body). Info/Path are
// pointers so an absent field marshals back out as null, not "".
type lockInfo struct {
	ID        string  `json:"ID"`
	Operation string  `json:"Operation"`
	Who       string  `json:"Who"`
	Info      *string `json:"Info"`
	Version   string  `json:"Version"`
	Created   string  `json:"Created"`
	Path      *string `json:"Path"`
}

type workspaceLock struct {
	ID        string  `json:"id"`
	Operation string  `json:"operation"`
	Who       string  `json:"who"`
	Info      *string `json:"info"`
	Version   string  `json:"version"`
	Created   string  `json:"created"`
	Path      *string `json:"path"`
}

type resourceTypeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

type workspaceStateData struct {
	Serial           int64               `json:"serial"`
	TerraformVersion string              `json:"terraform_version"`
	Lineage          string              `json:"lineage"`
	ResourceCount    int                 `json:"resource_count"`
	ResourceTypes    []resourceTypeCount `json:"resource_types"`
	Outputs          map[string]any      `json:"outputs"`
}

// workspaceView is the single shape the SPA consumes regardless of upstream status.
type workspaceView struct {
	IsEmpty bool                `json:"isEmpty"`
	Locked  bool                `json:"locked"`
	Lock    *workspaceLock      `json:"lock"`
	State   *workspaceStateData `json:"state"`
}

// fromEmpty builds the view for a workspace with no state yet (conductor 204).
func fromEmpty() workspaceView {
	return workspaceView{IsEmpty: true, Locked: false, Lock: nil, State: nil}
}

// fromLocked builds the view for a locked workspace (conductor 423), surfacing
// the lock holder/operation.
func fromLocked(raw lockInfo) workspaceView {
	return workspaceView{
		IsEmpty: false,
		Locked:  true,
		Lock: &workspaceLock{
			ID:        raw.ID,
			Operation: raw.Operation,
			Who:       raw.Who,
			Info:      raw.Info,
			Version:   raw.Version,
			Created:   raw.Created,
			Path:      raw.Path,
		},
		State: nil,
	}
}

// fromState builds the view for a populated workspace (conductor 200), tallying
// resources by type into a count summary sorted most-common first (ties broken
// by first appearance, matching the Node BFF's stable sort over insertion order).
func fromState(raw terraformState) workspaceView {
	counts := make(map[string]int)
	order := make([]string, 0)
	for _, r := range raw.Resources {
		if _, seen := counts[r.Type]; !seen {
			order = append(order, r.Type)
		}
		counts[r.Type]++
	}
	types := make([]resourceTypeCount, 0, len(order))
	for _, t := range order {
		types = append(types, resourceTypeCount{Type: t, Count: counts[t]})
	}
	sort.SliceStable(types, func(i, j int) bool { return types[i].Count > types[j].Count })

	return workspaceView{
		IsEmpty: false,
		Locked:  false,
		Lock:    nil,
		State: &workspaceStateData{
			Serial:           raw.Serial,
			TerraformVersion: raw.TerraformVersion,
			Lineage:          raw.Lineage,
			ResourceCount:    len(raw.Resources),
			ResourceTypes:    types,
			Outputs:          raw.Outputs,
		},
	}
}
