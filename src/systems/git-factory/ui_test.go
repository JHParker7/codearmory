package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The mini-portal is embedded, so a page that lost a feature's markup would still build
// and still serve — the failure would only show up in a browser. These assert the served
// bytes actually carry the surfaces the API grew.
func TestHandleUI_ServesTheEmbeddedPage(t *testing.T) {
	rec := httptest.NewRecorder()
	handleUI(rec, httptest.NewRequest(http.MethodGet, "/ui", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	// Redeploying the UI must not be masked by a cached copy.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("cache-control = %q, want no-cache", cc)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"data-review=",  // approve / request changes
		"data-comment=", // PR discussion
		"data-fork=",    // fork a repo
		"data-tag-new=", // create a tag
		"data-tag-del=", // delete a tag
		"data-search=",  // code search
		"checksView(",   // commit status checks
		"ownersView(",   // CODEOWNERS
		`data-tab="tags"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the served UI is missing %q", want)
		}
	}
}
