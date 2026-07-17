package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postPipeline creates a pipeline through the real handler and returns the
// recorder so a test can assert on status and body.
func postPipeline(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"u1"}`)
	r := httptest.NewRequest(http.MethodPost, "/pipelines", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, r)
	return w
}

// A pipeline authored as a graph round-trips: the routes are stored and returned.
func TestCreateWorkflow_AcceptsRoutes(t *testing.T) {
	body := `{
		"name":"graph-wf-ok",
		"steps":[
			{"action":"http","name":"build","with":{"service":"forge","path":"/x"}},
			{"action":"http","name":"publish","with":{"service":"forge","path":"/x"}},
			{"action":"http","name":"cleanup","with":{"service":"forge","path":"/x"}}
		],
		"routes":[
			{"from":"build","to":"publish","when":"steps.build.status == \"completed\""},
			{"from":"build","to":"cleanup","when":"steps.build.status == \"failed\""}
		]
	}`
	w := postPipeline(t, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d (%s), want 201", w.Code, w.Body.String())
	}
	var wf Workflow
	if err := json.Unmarshal(w.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wf.Routes) != 2 {
		t.Fatalf("routes = %v, want 2", wf.Routes)
	}
	if wf.Routes[0].From != "build" || wf.Routes[0].To != "publish" {
		t.Errorf("route[0] = %+v", wf.Routes[0])
	}
}

// Omitting routes stores none: a bare step array is a sequence, and its edges are
// derived as a chain at run time.
func TestCreateWorkflow_WithoutRoutesStoresNone(t *testing.T) {
	body := `{
		"name":"linear-wf-ok",
		"steps":[
			{"action":"http","name":"a","with":{"service":"forge","path":"/x"}},
			{"action":"http","name":"b","with":{"service":"forge","path":"/x"}},
			{"action":"http","name":"c","with":{"service":"forge","path":"/x"}}
		]
	}`
	w := postPipeline(t, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d (%s), want 201", w.Code, w.Body.String())
	}
	var wf Workflow
	if err := json.Unmarshal(w.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wf.Routes) != 0 {
		t.Fatalf("routes = %v, want none stored", wf.Routes)
	}
	// The graph is still derivable: a chain in array order.
	if got := routeSet(wf.buildGraph().routes); len(got) != 2 || got[0] != "a->b" || got[1] != "b->c" {
		t.Fatalf("derived routes = %v, want [a->b b->c]", got)
	}
}

func TestCreateWorkflow_RejectsBadRoutes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "cycle",
			body: `{"name":"wf-cycle","steps":[
				{"action":"http","name":"a","with":{"service":"f","path":"/x"}},
				{"action":"http","name":"b","with":{"service":"f","path":"/x"}}],
				"routes":[{"from":"a","to":"b"},{"from":"b","to":"a"}]}`,
			want: "cycle",
		},
		{
			name: "dangling route",
			body: `{"name":"wf-dangle","steps":[
				{"action":"http","name":"a","with":{"service":"f","path":"/x"}}],
				"routes":[{"from":"a","to":"ghost"}]}`,
			want: "unknown step",
		},
		{
			// A typo'd field is caught by compiling the condition against the
			// expression environment, at authoring time rather than mid-run.
			name: "typo in condition",
			body: `{"name":"wf-typo","steps":[
				{"action":"http","name":"a","with":{"service":"f","path":"/x"}},
				{"action":"http","name":"b","with":{"service":"f","path":"/x"}}],
				"routes":[{"from":"a","to":"b","when":"steps.a.stauts == \"completed\""}]}`,
			want: "route a->b",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postPipeline(t, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("got %d (%s), want 400", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body = %q, want it to contain %q", strings.TrimSpace(w.Body.String()), tc.want)
			}
		})
	}
}
