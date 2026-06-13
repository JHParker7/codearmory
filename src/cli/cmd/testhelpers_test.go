package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

// testUUID is a valid UUID used in tests that need to bypass name resolution
// (looksLikeUUID returns true, so no list call is made).
const testUUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}

// requestRecord captures one inbound HTTP request from the test server.
type requestRecord struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   []byte
}

// recordingServer starts an httptest.Server that captures every request and
// responds with the given status code and JSON body. The returned pointer is
// populated after each request.
func recordingServer(t *testing.T, statusCode int, responseBody string) (*httptest.Server, *requestRecord) {
	t.Helper()
	var rec requestRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.Method = r.Method
		rec.Path = r.URL.Path
		rec.Query = r.URL.RawQuery
		rec.Auth = r.Header.Get("Authorization")
		rec.Body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if responseBody != "" {
			w.Write([]byte(responseBody)) //nolint:errcheck
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &rec
}

// routeServer starts an httptest.Server backed by a ServeMux so individual
// tests can register per-path handlers.
func routeServer(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// setupCLI points the CLI at srv with a fixed bearer token and resets both
// on test cleanup.
func setupCLI(t *testing.T, srv *httptest.Server) {
	t.Helper()
	flagURL = srv.URL
	flagToken = "test-jwt"
	t.Cleanup(func() { flagURL = ""; flagToken = "" })
}

// setupCLINoToken points the CLI at srv without a token (for public endpoints).
func setupCLINoToken(t *testing.T, srv *httptest.Server) {
	t.Helper()
	flagURL = srv.URL
	flagToken = ""
	t.Cleanup(func() { flagURL = ""; flagToken = "" })
}

// silenceStdout redirects os.Stdout to /dev/null for the test duration so
// pretty-printed API responses don't pollute the test log.
func silenceStdout(t *testing.T) {
	t.Helper()
	orig := os.Stdout
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	os.Stdout = devNull
	t.Cleanup(func() { os.Stdout = orig; devNull.Close() })
}

// isolateHome sets HOME to a fresh temp directory so loadConfig / saveConfig
// don't touch the developer's real config file.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// jsonBody marshals v to compact JSON. Panics on error (test helper only).
func jsonBody(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
