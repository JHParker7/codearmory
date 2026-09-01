package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCombinedState_WorstFirst(t *testing.T) {
	mk := func(states ...string) []CommitStatus {
		s := make([]CommitStatus, 0, len(states))
		for _, st := range states {
			s = append(s, CommitStatus{State: st})
		}
		return s
	}
	cases := []struct {
		name string
		in   []CommitStatus
		want string
	}{
		{"none is empty", nil, ""},
		{"all success", mk(statusSuccess, statusSuccess), statusSuccess},
		{"a pending holds it pending", mk(statusSuccess, statusPending), statusPending},
		{"a failure beats pending", mk(statusPending, statusFailure, statusSuccess), statusFailure},
		{"an error beats failure", mk(statusFailure, statusError, statusPending), statusError},
	}
	for _, c := range cases {
		if got := combinedState(c.in); got != c.want {
			t.Errorf("%s: combinedState = %q, want %q", c.name, got, c.want)
		}
	}
}

// A re-post of the same context replaces its verdict (one row per context), a second
// context participates in the combined state, and an unknown state is rejected.
func TestCommitStatus_PostUpsertAndCombine(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	if err := connect().AutoMigrate(&CommitStatus{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	re := seedRepo(t, uuid.New().String(), "user-alice", "alice", "repo", "")
	newGatekeeperStubGrants(t, gatekeeperGrants{userID: "user-alice", username: "alice"})

	const sha = "abcdef1234567"
	post := func(context_, state string) int {
		body := `{"context":"` + context_ + `","state":"` + state + `"}`
		req := httptest.NewRequest(http.MethodPost, "/repos/"+re.ID+"/statuses/"+sha, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.SetPathValue("id", re.ID)
		req.SetPathValue("sha", sha)
		rec := httptest.NewRecorder()
		handlePostStatus(rec, req)
		return rec.Code
	}

	if code := post("ci", statusPending); code != http.StatusCreated {
		t.Fatalf("first post: status %d, want 201", code)
	}
	if code := post("ci", statusSuccess); code != http.StatusOK {
		t.Fatalf("re-post same context: status %d, want 200 (upsert)", code)
	}
	got := loadStatuses(context.Background(), re.ID, sha)
	if len(got) != 1 {
		t.Fatalf("context ci has %d rows, want 1 (upsert, not append)", len(got))
	}
	if got[0].State != statusSuccess {
		t.Errorf("ci state = %q, want the latest (success)", got[0].State)
	}

	if code := post("sec", statusFailure); code != http.StatusCreated {
		t.Fatalf("second context: status %d, want 201", code)
	}
	if s := combinedState(loadStatuses(context.Background(), re.ID, sha)); s != statusFailure {
		t.Errorf("combined = %q, want failure (one context failing)", s)
	}

	if code := post("ci", "bogus"); code != http.StatusBadRequest {
		t.Errorf("unknown state: status %d, want 400", code)
	}
}
