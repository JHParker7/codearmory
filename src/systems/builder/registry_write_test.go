package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type recordedReq struct {
	method string
	path   string
	body   map[string]any
	auth   string
}

// stubRegistry records requests and serves canned responses. listBody is the JSON
// returned for GET /services.
type stubRegistry struct {
	mu       sync.Mutex
	reqs     []recordedReq
	listBody string
}

func (s *stubRegistry) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				json.Unmarshal(raw, &body) //nolint:errcheck
			}
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, recordedReq{r.Method, r.URL.Path, body, r.Header.Get("X-Service-Key")})
		s.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/services":
			lb := s.listBody
			if lb == "" {
				lb = "[]"
			}
			w.Write([]byte(lb)) //nolint:errcheck
		case r.Method == http.MethodPost && r.URL.Path == "/services":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"service_id":"svc-1"}`)) //nolint:errcheck
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/endpoints"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/services/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/service-accounts":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *stubRegistry) calls() []recordedReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedReq(nil), s.reqs...)
}

// useStubRegistry points the package registry globals at the stub and restores them.
func useStubRegistry(t *testing.T, stub *stubRegistry) {
	t.Helper()
	srv := stub.server(t)
	origURL, origKey, origClient := registryURL, getRegistryKey, httpClient
	registryURL = srv.URL
	getRegistryKey = func() string { return "rk" }
	httpClient = initHTTPClient()
	t.Cleanup(func() {
		registryURL, getRegistryKey, httpClient = origURL, origKey, origClient
	})
}

func TestEnsureRegistryService_RegistersWhenAbsent(t *testing.T) {
	stub := &stubRegistry{listBody: "[]"}
	useStubRegistry(t, stub)
	def, _ := embeddedServiceDef("tickets")

	if err := ensureRegistryService(context.Background(), def, "codearmory"); err != nil {
		t.Fatalf("ensureRegistryService: %v", err)
	}
	calls := stub.calls()
	var get, post, put *recordedReq
	for i := range calls {
		switch {
		case calls[i].method == http.MethodGet && calls[i].path == "/services":
			get = &calls[i]
		case calls[i].method == http.MethodPost && calls[i].path == "/services":
			post = &calls[i]
		case calls[i].method == http.MethodPut && strings.HasSuffix(calls[i].path, "/endpoints"):
			put = &calls[i]
		}
	}
	if get == nil || post == nil || put == nil {
		t.Fatalf("expected GET+POST+PUT, got %+v", calls)
	}
	if post.auth != "builder:rk" {
		t.Errorf("auth = %q, want builder:rk", post.auth)
	}
	if post.body["name"] != "tickets" {
		t.Errorf("POST name = %v, want tickets", post.body["name"])
	}
	if url, _ := post.body["url"].(string); url != "http://codearmory-tickets:8086" {
		t.Errorf("POST url = %q, want http://codearmory-tickets:8086", url)
	}
	if put.path != "/services/svc-1/endpoints" {
		t.Errorf("PUT path = %q, want /services/svc-1/endpoints", put.path)
	}
	if dg, ok := put.body["default_grants"].([]any); !ok || len(dg) == 0 {
		t.Errorf("PUT body missing default_grants: %+v", put.body["default_grants"])
	}
	if eps, ok := put.body["endpoints"].([]any); !ok || len(eps) == 0 {
		t.Error("PUT body missing endpoints")
	}
}

func TestEnsureRegistryService_NoChurnWhenActive(t *testing.T) {
	// Already active with endpoints → only the lookup GET, no POST/PUT.
	stub := &stubRegistry{listBody: `[{"service_id":"svc-1","name":"tickets","endpoints":[{"method":"GET","path":"/x"}]}]`}
	useStubRegistry(t, stub)
	def, _ := embeddedServiceDef("tickets")

	if err := ensureRegistryService(context.Background(), def, "codearmory"); err != nil {
		t.Fatalf("ensureRegistryService: %v", err)
	}
	for _, c := range stub.calls() {
		if c.method != http.MethodGet {
			t.Errorf("expected only GET, saw %s %s", c.method, c.path)
		}
	}
}

func TestRemoveRegistryService_DeletesByID(t *testing.T) {
	stub := &stubRegistry{listBody: `[{"service_id":"svc-9","name":"forge","endpoints":[{"method":"GET","path":"/x"}]}]`}
	useStubRegistry(t, stub)

	if err := removeRegistryService(context.Background(), "forge"); err != nil {
		t.Fatalf("removeRegistryService: %v", err)
	}
	var deleted bool
	for _, c := range stub.calls() {
		if c.method == http.MethodDelete && c.path == "/services/svc-9" {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("expected DELETE /services/svc-9, got %+v", stub.calls())
	}
}

func TestRemoveRegistryService_NoopWhenAbsent(t *testing.T) {
	stub := &stubRegistry{listBody: "[]"}
	useStubRegistry(t, stub)
	if err := removeRegistryService(context.Background(), "forge"); err != nil {
		t.Fatalf("removeRegistryService: %v", err)
	}
	for _, c := range stub.calls() {
		if c.method == http.MethodDelete {
			t.Error("should not DELETE when service is absent")
		}
	}
}

func TestEnsureRegistryAccount_Upserts(t *testing.T) {
	stub := &stubRegistry{}
	useStubRegistry(t, stub)
	if err := ensureRegistryAccount(context.Background(), "tickets", "derived-key"); err != nil {
		t.Fatalf("ensureRegistryAccount: %v", err)
	}
	var got *recordedReq
	cs := stub.calls()
	for i := range cs {
		if cs[i].method == http.MethodPost && cs[i].path == "/service-accounts" {
			got = &cs[i]
		}
	}
	if got == nil {
		t.Fatalf("expected POST /service-accounts, got %+v", stub.calls())
	}
	if got.body["name"] != "tickets" || got.body["key"] != "derived-key" || got.body["role"] != "read" {
		t.Errorf("unexpected account body: %+v", got.body)
	}
}

func TestRegisterServicesOn_GatedByConfig(t *testing.T) {
	origURL, origKey := registryURL, getRegistryKey
	t.Cleanup(func() { registryURL, getRegistryKey = origURL, origKey })

	registryURL, getRegistryKey = "", nil
	if registerServicesOn() {
		t.Error("should be off when registry is unconfigured")
	}
	registryURL, getRegistryKey = "http://r", func() string { return "k" }
	if !registerServicesOn() {
		t.Error("should be on when configured")
	}
	t.Setenv("BUILDER_REGISTER_SERVICES", "false")
	if registerServicesOn() {
		t.Error("should be off when BUILDER_REGISTER_SERVICES=false")
	}
}
