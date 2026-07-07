package main

import (
	"encoding/json"
	"testing"
)

func TestFromEmpty(t *testing.T) {
	v := fromEmpty()
	if !v.IsEmpty || v.Locked || v.Lock != nil || v.State != nil {
		t.Fatalf("unexpected empty view: %+v", v)
	}
}

func TestFromLocked(t *testing.T) {
	v := fromLocked(lockInfo{
		ID: "abc", Operation: "OperationTypeApply", Who: "jane@host",
		Info: strptr("deploying"), Version: "1.7.0", Created: "2026-07-07T00:00:00Z",
		Path: strptr("workspaces/x"),
	})
	if v.IsEmpty || !v.Locked || v.State != nil || v.Lock == nil {
		t.Fatalf("unexpected locked view: %+v", v)
	}
	if v.Lock.ID != "abc" || v.Lock.Who != "jane@host" || *v.Lock.Info != "deploying" {
		t.Fatalf("lock fields not mapped: %+v", v.Lock)
	}
}

func TestFromLocked_NilInfoPath(t *testing.T) {
	v := fromLocked(lockInfo{ID: "1", Operation: "op", Who: "w", Version: "v", Created: "c"})
	if v.Lock.Info != nil || v.Lock.Path != nil {
		t.Fatalf("absent Info/Path should map to nil (marshals null): %+v", v.Lock)
	}
	// Confirm they serialize as JSON null, not "".
	b, _ := json.Marshal(v.Lock)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["info"] != nil || m["path"] != nil {
		t.Fatalf("expected null info/path, got %v", string(b))
	}
}

func TestFromState_CountsAndSort(t *testing.T) {
	v := fromState(terraformState{
		TerraformVersion: "1.7.0", Serial: 42, Lineage: "lin",
		Outputs: map[string]any{"ip": "1.2.3.4"},
		Resources: []terraformResource{
			{Type: "aws_instance"},
			{Type: "aws_s3_bucket"},
			{Type: "aws_instance"},
			{Type: "aws_instance"},
			{Type: "aws_s3_bucket"},
		},
	})
	if v.State == nil || v.State.ResourceCount != 5 {
		t.Fatalf("expected 5 resources, got %+v", v.State)
	}
	if v.State.Serial != 42 || v.State.TerraformVersion != "1.7.0" {
		t.Fatalf("scalar fields not mapped: %+v", v.State)
	}
	// Sorted most-common first: aws_instance(3) then aws_s3_bucket(2).
	got := v.State.ResourceTypes
	if len(got) != 2 || got[0].Type != "aws_instance" || got[0].Count != 3 || got[1].Count != 2 {
		t.Fatalf("resource type tally wrong: %+v", got)
	}
}

func TestFromState_Empty(t *testing.T) {
	v := fromState(terraformState{})
	if v.State == nil || v.State.ResourceCount != 0 || len(v.State.ResourceTypes) != 0 {
		t.Fatalf("empty state should have zero resources: %+v", v.State)
	}
}
