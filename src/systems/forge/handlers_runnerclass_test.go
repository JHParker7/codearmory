package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// TestRunnerClass_BackendAndDisk_DB checks the new backend/disk_gb fields are
// accepted, persisted, and updatable through the runner-class API — the knob that
// lets an admin point a class at a non-default runtime.
func TestRunnerClass_BackendAndDisk_DB(t *testing.T) {
	requireForgeDB(t)
	name := "rc-" + uuid.New().String()
	t.Cleanup(func() { connect().Exec(`DELETE FROM runner_classes WHERE name = ?`, name) }) //nolint:errcheck

	authAs(t, "admin")
	createBody := `{"name":"` + name + `","memory_mb":4096,"cpu_millicores":2000,` +
		`"pids_limit":128,"tmpfs_mb":64,"disk_gb":50,"backend":"proxmox-prod","enabled":true}`
	r := httptest.NewRequest(http.MethodPost, "/runner-classes", bytes.NewBufferString(createBody))
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleCreateRunnerClass(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", w.Code, w.Body.String())
	}
	var created RunnerClass
	json.NewDecoder(w.Body).Decode(&created) //nolint:errcheck
	if created.Backend != "proxmox-prod" {
		t.Errorf("backend = %q, want proxmox-prod", created.Backend)
	}
	if created.DiskGB != 50 {
		t.Errorf("disk_gb = %d, want 50", created.DiskGB)
	}

	// Omitting backend on update defaults it back to "default".
	authAs(t, "admin")
	updBody := `{"memory_mb":4096,"cpu_millicores":2000,"pids_limit":128,"tmpfs_mb":64,"disk_gb":80,"enabled":true}`
	r = httptest.NewRequest(http.MethodPut, "/runner-classes/"+name, bytes.NewBufferString(updBody))
	r.SetPathValue("name", name)
	r.Header.Set("Authorization", "Bearer t")
	w = httptest.NewRecorder()
	handleUpdateRunnerClass(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("update: got %d, want 200: %s", w.Code, w.Body.String())
	}

	authAs(t, "admin")
	r = httptest.NewRequest(http.MethodGet, "/runner-classes/"+name, nil)
	r.SetPathValue("name", name)
	r.Header.Set("Authorization", "Bearer t")
	w = httptest.NewRecorder()
	handleGetRunnerClass(w, r)
	var got RunnerClass
	json.NewDecoder(w.Body).Decode(&got) //nolint:errcheck
	if got.Backend != "default" {
		t.Errorf("backend after update = %q, want default (omitted → default)", got.Backend)
	}
	if got.DiskGB != 80 {
		t.Errorf("disk_gb after update = %d, want 80", got.DiskGB)
	}
}

// TestRunnerClass_Privileged_DB checks the privileged opt-in is accepted and
// persisted on a kata backend, but rejected on a non-VM (container) backend —
// the user-facing half of the guard whose runtime half lives in buildJob.
func TestRunnerClass_Privileged_DB(t *testing.T) {
	requireForgeDB(t)
	kataBackend := "be-" + uuid.New().String()
	gvisorBackend := "be-" + uuid.New().String()
	dockerBackend := "be-" + uuid.New().String()
	className := "rc-" + uuid.New().String()
	gvisorClassName := "rc-" + uuid.New().String()
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM runner_classes WHERE name IN (?, ?)`, className, gvisorClassName)                     //nolint:errcheck
		connect().Exec(`DELETE FROM runtime_backends WHERE name IN (?, ?, ?)`, kataBackend, gvisorBackend, dockerBackend) //nolint:errcheck
	})

	mkBackend := func(body string) {
		authAs(t, "admin")
		r := httptest.NewRequest(http.MethodPost, "/runtime-backends", bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer t")
		w := httptest.NewRecorder()
		handleCreateRuntimeBackend(w, r)
		if w.Code != http.StatusCreated {
			t.Fatalf("create backend: got %d, want 201: %s", w.Code, w.Body.String())
		}
	}
	mkBackend(`{"name":"` + kataBackend + `","type":"kata","enabled":true,"config":{"runtime_class":"kata"}}`)
	mkBackend(`{"name":"` + gvisorBackend + `","type":"gvisor","enabled":true,"config":{"runtime_class":"gvisor"}}`)
	mkBackend(`{"name":"` + dockerBackend + `","type":"docker","enabled":true,"config":{}}`)

	// privileged on a kata backend is accepted and persisted.
	authAs(t, "admin")
	okBody := `{"name":"` + className + `","memory_mb":2048,"cpu_millicores":1000,"pids_limit":64,` +
		`"tmpfs_mb":256,"disk_gb":10,"backend":"` + kataBackend + `","enabled":true,"privileged":true}`
	r := httptest.NewRequest(http.MethodPost, "/runner-classes", bytes.NewBufferString(okBody))
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	handleCreateRunnerClass(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create privileged kata class: got %d, want 201: %s", w.Code, w.Body.String())
	}
	var created RunnerClass
	json.NewDecoder(w.Body).Decode(&created) //nolint:errcheck
	if !created.Privileged {
		t.Error("privileged should be persisted as true on a kata backend")
	}

	// privileged on a gvisor backend is also accepted — gVisor is kernel-isolated
	// (the Sentry contains root), so root + writable rootfs is safe just as on kata.
	authAs(t, "admin")
	gvBody := `{"name":"` + gvisorClassName + `","memory_mb":2048,"cpu_millicores":1000,"pids_limit":64,` +
		`"tmpfs_mb":256,"disk_gb":10,"backend":"` + gvisorBackend + `","enabled":true,"privileged":true}`
	r = httptest.NewRequest(http.MethodPost, "/runner-classes", bytes.NewBufferString(gvBody))
	r.Header.Set("Authorization", "Bearer t")
	w = httptest.NewRecorder()
	handleCreateRunnerClass(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create privileged gvisor class: got %d, want 201: %s", w.Code, w.Body.String())
	}

	// privileged on a non-kernel-isolated (container) backend is rejected — never inserted.
	authAs(t, "admin")
	badBody := `{"name":"rc-` + uuid.New().String() + `","memory_mb":2048,"cpu_millicores":1000,"pids_limit":64,` +
		`"tmpfs_mb":256,"disk_gb":10,"backend":"` + dockerBackend + `","enabled":true,"privileged":true}`
	r = httptest.NewRequest(http.MethodPost, "/runner-classes", bytes.NewBufferString(badBody))
	r.Header.Set("Authorization", "Bearer t")
	w = httptest.NewRecorder()
	handleCreateRunnerClass(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create privileged docker class: got %d, want 400: %s", w.Code, w.Body.String())
	}
}
