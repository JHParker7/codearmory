package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ── parseAllowlist ────────────────────────────────────────────────────────────

func TestParseAllowlist_Empty(t *testing.T) {
	if al := parseAllowlist(""); al != nil {
		t.Fatalf("expected nil for empty string, got %v", al)
	}
}

func TestParseAllowlist_Single(t *testing.T) {
	al := parseAllowlist("github.com")
	if len(al) != 1 || al[0] != "github.com" {
		t.Fatalf("unexpected allowlist: %v", al)
	}
}

func TestParseAllowlist_Multiple(t *testing.T) {
	al := parseAllowlist("github.com,registry.k8s.io, quay.io")
	if len(al) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(al), al)
	}
	if al[2] != "quay.io" {
		t.Fatalf("trimming failed: got %q", al[2])
	}
}

func TestParseAllowlist_SpacesOnly(t *testing.T) {
	if al := parseAllowlist("  ,  ,  "); al != nil {
		t.Fatalf("expected nil for whitespace-only entries, got %v", al)
	}
}

// ── allowlist.permits ─────────────────────────────────────────────────────────

func TestPermits_ExactMatch(t *testing.T) {
	al := allowlist{"github.com"}
	if !al.permits("github.com") {
		t.Fatal("exact host should be permitted")
	}
}

func TestPermits_ExactNoMatch(t *testing.T) {
	al := allowlist{"github.com"}
	if al.permits("evil.com") {
		t.Fatal("unrelated host should not be permitted")
	}
}

func TestPermits_WildcardSubdomain(t *testing.T) {
	al := allowlist{"*.github.com"}
	if !al.permits("api.github.com") {
		t.Fatal("subdomain should be permitted by wildcard")
	}
	if !al.permits("raw.github.com") {
		t.Fatal("subdomain should be permitted by wildcard")
	}
}

func TestPermits_WildcardNoMatchApex(t *testing.T) {
	al := allowlist{"*.github.com"}
	if al.permits("github.com") {
		t.Fatal("apex domain should not match wildcard entry")
	}
}

func TestPermits_WildcardNoMatchUnrelated(t *testing.T) {
	al := allowlist{"*.github.com"}
	if al.permits("evil.com") {
		t.Fatal("unrelated host should not match wildcard")
	}
}

func TestPermits_EmptyAllowlist(t *testing.T) {
	var al allowlist
	if al.permits("github.com") {
		t.Fatal("empty allowlist should block all")
	}
}

func TestPermits_MultipleEntries(t *testing.T) {
	al := allowlist{"github.com", "*.docker.io"}
	if !al.permits("github.com") {
		t.Fatal("exact match in multi-entry allowlist")
	}
	if !al.permits("registry-1.docker.io") {
		t.Fatal("wildcard match in multi-entry allowlist")
	}
	if al.permits("evil.com") {
		t.Fatal("unlisted host should not be permitted")
	}
}

// "*" is the public-only sentinel: it passes the hostname check for ANY host, so an
// operator can grant broad outbound access (e.g. all of AWS) without enumerating
// domains. The dial-time IP guard still applies (see TestHandleHTTP_StarStillBlocksInternalIP).
func TestPermits_StarAllowsAnyHost(t *testing.T) {
	al := parseAllowlist("*")
	for _, h := range []string{"github.com", "sts.amazonaws.com", "anything.example.org"} {
		if !al.permits(h) {
			t.Errorf("permits(%q) = false, want true under \"*\"", h)
		}
	}
}

// "*" wins even when mixed with explicit entries.
func TestPermits_StarAmongEntries(t *testing.T) {
	if !parseAllowlist("github.com,*").permits("unlisted.example") {
		t.Error("\"*\" in the list should permit any host")
	}
}

func TestAllowsAll(t *testing.T) {
	if !parseAllowlist("*").allowsAll() {
		t.Error("allowsAll() = false for \"*\"")
	}
	if !parseAllowlist("a.com, *").allowsAll() {
		t.Error("allowsAll() = false when \"*\" is present among entries")
	}
	if parseAllowlist("github.com,*.docker.io").allowsAll() {
		t.Error("allowsAll() = true for a normal allowlist (a *.suffix wildcard is not the \"*\" sentinel)")
	}
	if parseAllowlist("").allowsAll() {
		t.Error("allowsAll() = true for an empty allowlist")
	}
}

// ── proxy.ServeHTTP ───────────────────────────────────────────────────────────

func TestServeHTTP_Healthz(t *testing.T) {
	p := &proxy{al: allowlist{"example.com"}}
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

func TestServeHTTP_RelativeNonHealthzReturns400(t *testing.T) {
	p := &proxy{al: allowlist{"example.com"}}
	r := httptest.NewRequest(http.MethodGet, "/some/other/path", nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestServeHTTP_PostRelativeReturns400(t *testing.T) {
	p := &proxy{al: allowlist{"example.com"}}
	r := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	// POST /healthz is not the health-check path (requires GET) so it's a relative non-absolute URL → 400.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

// ── proxy.handleHTTP ──────────────────────────────────────────────────────────

func TestHandleHTTP_ForbiddenHost(t *testing.T) {
	p := &proxy{al: allowlist{"allowed.com"}}
	r := httptest.NewRequest(http.MethodGet, "http://forbidden.com/path", nil)
	w := httptest.NewRecorder()
	p.handleHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestHandleHTTP_AllowedHost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	// httptest binds to loopback; relax the resolved-IP guard so this test can reach it.
	defer func(f func(net.IP) bool) { allowDialIP = f }(allowDialIP)
	allowDialIP = func(net.IP) bool { return true }

	// upstream.URL is "http://127.0.0.1:PORT"; net.SplitHostPort extracts "127.0.0.1".
	p := &proxy{al: allowlist{"127.0.0.1"}}
	r := httptest.NewRequest(http.MethodGet, upstream.URL+"/ok", nil)
	w := httptest.NewRecorder()
	p.handleHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204", w.Code)
	}
}

// Public-only mode ("*") passes the hostname check for any host, but the dial-time IP
// guard still runs: a host resolving to a loopback/private/metadata address is blocked
// (the dial fails → 502), proving "*" grants PUBLIC destinations only, never internal
// ones. allowDialIP is deliberately NOT relaxed here (unlike TestHandleHTTP_AllowedHost)
// so the real isDisallowedIP guard rejects the httptest loopback upstream.
func TestHandleHTTP_StarStillBlocksInternalIP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	p := &proxy{al: allowlist{"*"}}
	r := httptest.NewRequest(http.MethodGet, upstream.URL+"/ok", nil)
	w := httptest.NewRecorder()
	p.handleHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502 — the \"*\" name match must NOT bypass the IP guard for a loopback upstream", w.Code)
	}
}

func TestHandleHTTP_HopByHopHeadersStripped(t *testing.T) {
	var receivedConnection string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedConnection = r.Header.Get("Connection")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// httptest binds to loopback; relax the resolved-IP guard so this test can reach it.
	defer func(f func(net.IP) bool) { allowDialIP = f }(allowDialIP)
	allowDialIP = func(net.IP) bool { return true }

	p := &proxy{al: allowlist{"127.0.0.1"}}
	r := httptest.NewRequest(http.MethodGet, upstream.URL+"/check", nil)
	r.Header.Set("Connection", "keep-alive")
	w := httptest.NewRecorder()
	p.handleHTTP(w, r)
	if receivedConnection != "" {
		t.Fatalf("hop-by-hop header Connection should be stripped, got %q", receivedConnection)
	}
}

// ── isDisallowedIP ────────────────────────────────────────────────────────────

func TestIsDisallowedIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"169.254.169.254", true},       // cloud metadata (link-local)
		{"127.0.0.1", true},             // loopback
		{"10.0.0.5", true},              // private
		{"192.168.1.1", true},           // private
		{"172.16.0.1", true},            // private
		{"::1", true},                   // loopback (v6)
		{"0.0.0.0", true},               // unspecified
		{"8.8.8.8", false},              // public
		{"1.1.1.1", false},              // public
		{"2606:4700:4700::1111", false}, // public (v6)
	}
	for _, tc := range cases {
		if got := isDisallowedIP(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("isDisallowedIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// Ranges the dial-time guard must deny to match the chart's kata egress
// NetworkPolicy. In public-only mode ("*") this guard is the only egress boundary
// for the docker/kubernetes/gvisor backends, so a gap here is reachable
// internal-network access from a sandbox.
func TestIsDisallowedIP_InternalRanges(t *testing.T) {
	cases := []struct {
		ip     string
		want   bool
		reason string
	}{
		// CGNAT 100.64.0.0/10 — pod/node network on EKS/GKE/OKE, and Alibaba metadata.
		{"100.64.0.1", true, "CGNAT low edge"},
		{"100.100.100.200", true, "Alibaba Cloud metadata"},
		{"100.127.255.255", true, "CGNAT high edge"},
		{"100.63.255.255", false, "just below CGNAT — public"},
		{"100.128.0.0", false, "just above CGNAT — public"},
		// 0.0.0.0/8 — "this network", non-routable beyond the unspecified address.
		{"0.1.2.3", true, "0.0.0.0/8"},
		// Reserved / broadcast.
		{"255.255.255.255", true, "broadcast"},
		{"240.0.0.1", true, "reserved 240.0.0.0/4"},
		// IPv6 unique-local and the deprecated site-local range.
		{"fd00::1", true, "IPv6 unique-local fc00::/7"},
		{"fc00::1", true, "IPv6 unique-local fc00::/7"},
		{"fec0::1", true, "deprecated IPv6 site-local"},
		{"fe80::1", true, "IPv6 link-local"},
		// IPv4-mapped IPv6 must not bypass the IPv4 checks.
		{"::ffff:10.0.0.1", true, "IPv4-mapped private"},
		{"::ffff:127.0.0.1", true, "IPv4-mapped loopback"},
		{"::ffff:169.254.169.254", true, "IPv4-mapped metadata"},
		{"::ffff:100.64.0.1", true, "IPv4-mapped CGNAT"},
		{"::ffff:8.8.8.8", false, "IPv4-mapped public"},
		// NAT64 addresses are judged by the IPv4 address they embed.
		{"64:ff9b::a00:1", true, "NAT64-embedded 10.0.0.1"},
		{"64:ff9b::a9fe:a9fe", true, "NAT64-embedded 169.254.169.254"},
		{"64:ff9b::808:808", false, "NAT64-embedded public 8.8.8.8"},
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("test case %q is not a valid IP", tc.ip)
		}
		if got := isDisallowedIP(ip); got != tc.want {
			t.Errorf("isDisallowedIP(%s) = %v, want %v (%s)", tc.ip, got, tc.want, tc.reason)
		}
	}
}

func TestIsDisallowedIP_Nil(t *testing.T) {
	if !isDisallowedIP(nil) {
		t.Fatal("a nil IP must fail closed")
	}
}
