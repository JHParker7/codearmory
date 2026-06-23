package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestHandleListOutposts_Denied(t *testing.T) {
	stubGatekeeper(t, "u1", "", false) // authorized:false → SDK writes 403
	w := httptest.NewRecorder()
	handleListOutposts(w, bearerReq(http.MethodGet, "/outposts", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestHandleGetOutpost_Denied(t *testing.T) {
	stubGatekeeper(t, "u1", "", false)
	id := uuid.New().String()
	r := bearerReq(http.MethodGet, "/outposts/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetOutpost(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestHandleDeleteOutpost_Denied(t *testing.T) {
	stubGatekeeper(t, "u1", "", false)
	id := uuid.New().String()
	r := bearerReq(http.MethodDelete, "/outposts/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeleteOutpost(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestHandleDeleteOutpost_NotFound(t *testing.T) {
	requireDB(t)
	stubGatekeeper(t, "u1", "", true) // authorized, but the outpost does not exist
	id := uuid.New().String()
	r := bearerReq(http.MethodDelete, "/outposts/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleDeleteOutpost(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestHandleGetOutpost_DeniedThenNotFoundDistinct(t *testing.T) {
	requireDB(t)
	// authorized but missing → 404 (distinct from the denied 403 above).
	stubGatekeeper(t, "u1", "", true)
	id := uuid.New().String()
	r := bearerReq(http.MethodGet, "/outposts/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handleGetOutpost(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}
