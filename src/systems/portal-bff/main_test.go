package main

import (
	"net/http"
	"os"
	"testing"
)

// TestMain wires the package-level HTTP client and metric instruments so
// handler tests can run without the full main() bootstrap.
func TestMain(m *testing.M) {
	httpClient = &http.Client{}
	initMetrics()
	os.Exit(m.Run())
}

func strptr(s string) *string { return &s }
