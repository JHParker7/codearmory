package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// paramTypeFor classifies a path param by name: {id} is a UUID, and the slug-typed
// names (username/workspace/org/team — the blueprints state route; ns — owner-first
// per-record routes) get the slug regex. Any other name is unvalidated (e.g. forge's
// runner-class {name}), so conductor forwards it and the backend decides.
func TestParamTypeFor(t *testing.T) {
	for _, name := range []string{"ns", "username", "workspace", "org", "team"} {
		if got := paramTypeFor(name); got != "slug" {
			t.Errorf("paramTypeFor(%q) = %q, want slug", name, got)
		}
	}
	if got := paramTypeFor("id"); got != "uuid" {
		t.Errorf("paramTypeFor(id) = %q, want uuid", got)
	}
	// "namespace" must stay unvalidated: the container registry routes use it for a
	// docker namespace, which legitimately contains characters the slug pattern
	// rejects. Constraining it here would start 400ing pulls.
	for _, name := range []string{"name", "owner", "index", "namespace", ""} {
		if got := paramTypeFor(name); got != "" {
			t.Errorf("paramTypeFor(%q) = %q, want unvalidated", name, got)
		}
	}
}

// Owner-first per-record resources put the OWNER's namespace at the front of the
// resource string ("{ns}/tickets/tickets/{id}"), which is what makes gatekeeper
// evaluate the check against the record's owner instead of silently re-scoping it to
// whoever asked. That only works if conductor substitutes {ns} as readily as {id} —
// otherwise the literal "{ns}" reaches gatekeeper and matches no grant.
func TestResolveResourceOwnerFirst(t *testing.T) {
	got := resolveResource("{ns}/tickets/tickets/{id}",
		[]string{"ns", "id"}, []string{"alice", "abc-123"})
	if want := "alice/tickets/tickets/abc-123"; got != want {
		t.Errorf("resolveResource = %q, want %q", got, want)
	}

	// The legacy caller-scoped form has to keep resolving unchanged — both shapes are
	// served during the migration.
	got = resolveResource("tickets/tickets/{id}", []string{"id"}, []string{"abc-123"})
	if want := "tickets/tickets/abc-123"; got != want {
		t.Errorf("legacy resolveResource = %q, want %q", got, want)
	}
}

// validatePathParams rejects malformed slug/UUID segments with 400 before any forward,
// and normalises UUIDs. This is conductor's input-validation gate (previously covered
// at the integration level via the blueprints /state/{username}/{workspace} route).
func TestValidatePathParams(t *testing.T) {
	cases := []struct {
		desc   string
		names  []string
		values []string
		ok     bool
	}{
		{"valid slugs", []string{"username", "workspace"}, []string{"alice", "dev"}, true},
		{"slug with hyphen and underscore", []string{"username"}, []string{"my-user_123"}, true},
		{"slug with special char rejected", []string{"username"}, []string{"bad!user"}, false},
		{"second slug special char rejected", []string{"username", "workspace"}, []string{"alice", "bad!ws"}, false},
		{"slug too long rejected", []string{"workspace"}, []string{strings.Repeat("a", 65)}, false},
		{"non-slug param name is not validated", []string{"name"}, []string{"bad!name"}, true},
		{"valid hyphenated uuid", []string{"id"}, []string{"d28c1a9a-6c7a-4e35-9e95-cb63cc680b48"}, true},
		{"valid hex uuid normalised", []string{"id"}, []string{"d28c1a9a6c7a4e359e95cb63cc680b48"}, true},
		{"non-uuid id rejected", []string{"id"}, []string{"not-a-uuid"}, false},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		out, ok := validatePathParams(rec, c.names, c.values)
		if ok != c.ok {
			t.Errorf("%s: ok=%v, want %v (status %d)", c.desc, ok, c.ok, rec.Code)
			continue
		}
		if !c.ok && rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400", c.desc, rec.Code)
		}
		if c.ok && c.desc == "valid hex uuid normalised" && out[0] != "d28c1a9a-6c7a-4e35-9e95-cb63cc680b48" {
			t.Errorf("hex uuid not normalised to hyphenated form: %q", out[0])
		}
	}
}

// readAndValidateBody's default cap is a JSON-API limit, not a transfer limit. An
// endpoint that carries bulk content declares max_body_bytes in the registry manifest;
// without that, the gateway buffered every POST/PUT to measure it and answered 413 at
// 64 KiB — which made artifact upload (sized against a multi-gigabyte per-user quota
// the artifacts service enforces itself) impossible through conductor.
func TestReadAndValidateBodyLimits(t *testing.T) {
	big := strings.Repeat("x", maxRequestBodyBytes+1)

	newReq := func(body string) *http.Request {
		r := httptest.NewRequest(http.MethodPut, "/artifacts/cache", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/octet-stream")
		return r
	}

	t.Run("default cap still rejects an oversized body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if _, ok := readAndValidateBody(rec, newReq(big), endpointEntry{}); ok {
			t.Fatal("oversized body accepted under the default cap")
		}
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", rec.Code)
		}
	})

	t.Run("unlimited streams the body through unbuffered", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := newReq(big)
		buffered, ok := readAndValidateBody(rec, r, endpointEntry{maxBodyBytes: -1})
		if !ok {
			t.Fatalf("body rejected, status %d", rec.Code)
		}
		// nil, not the bytes: an upload must never be read into conductor's memory.
		if buffered != nil {
			t.Errorf("body was buffered (%d bytes); it must be streamed", len(buffered))
		}
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read forwarded body: %v", err)
		}
		if len(got) != len(big) {
			t.Errorf("forwarded %d bytes, want %d", len(got), len(big))
		}
	})

	t.Run("explicit cap rejects a declared Content-Length over it", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if _, ok := readAndValidateBody(rec, newReq(big), endpointEntry{maxBodyBytes: 128}); ok {
			t.Fatal("body over the declared cap accepted")
		}
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", rec.Code)
		}
	})

	t.Run("explicit cap enforced when Content-Length is absent", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := newReq(big)
		r.ContentLength = -1 // chunked: the header cannot be trusted to pre-screen
		if _, ok := readAndValidateBody(rec, r, endpointEntry{maxBodyBytes: 128}); !ok {
			t.Fatalf("body rejected up front, status %d", rec.Code)
		}
		if _, err := io.ReadAll(r.Body); err == nil {
			t.Error("body read past the cap without error; MaxBytesReader is not enforcing")
		}
	})

	t.Run("JSON validation still runs on the default path", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{not json"))
		r.Header.Set("Content-Type", "application/json")
		if _, ok := readAndValidateBody(rec, r, endpointEntry{}); ok {
			t.Fatal("malformed JSON accepted")
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}
