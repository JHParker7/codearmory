package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/code-armory-app/codearmory_sdk/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// allowlist holds domain patterns permitted for outbound connections.
// Each entry is either an exact hostname ("github.com") or a wildcard
// prefix ("*.github.com") that matches any subdomain.
type allowlist []string

func parseAllowlist(s string) allowlist {
	if s == "" {
		return nil
	}
	var al allowlist
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			al = append(al, p)
		}
	}
	return al
}

func (al allowlist) permits(host string) bool {
	for _, pattern := range al {
		// "*" alone is the public-only sentinel: any hostname passes the NAME check.
		// This is NOT "allow everything" — the IP guard (safeDialContext →
		// isDisallowedIP) still runs at dial time on every request, so the proxy will
		// reach any PUBLIC address but never a loopback/private/link-local/metadata
		// one. It lets an operator grant broad outbound access (e.g. all of AWS)
		// without enumerating domains, while keeping SSRF/internal access blocked.
		if pattern == "*" {
			return true
		}
		if pattern == host {
			return true
		}
		// "*.example.com" matches "api.example.com" but not "example.com" itself.
		if strings.HasPrefix(pattern, "*.") && strings.HasSuffix(host, pattern[1:]) {
			return true
		}
	}
	return false
}

// allowsAll reports whether the allowlist is in public-only mode (the "*" sentinel).
// Even then the dial-time IP guard still blocks private/internal/metadata addresses.
func (al allowlist) allowsAll() bool {
	return slices.Contains(al, "*")
}

// hopByHop headers must not be forwarded between proxy and upstream.
var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"proxy-connection":    true,
	"te":                  true,
	"trailers":            true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"x-forwarded-for":     true,
}

var fwdClient = &http.Client{
	Timeout: 60 * time.Second,
	// Return 3xx responses to the client rather than following them here.
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
	// Dial through safeDialContext so the resolved IP is validated, not just the
	// allowlisted hostname.
	Transport: &http.Transport{DialContext: safeDialContext},
}

// mustCIDR parses a CIDR literal, panicking on a malformed one. Only used for the
// fixed table below, so a bad entry is a programming error caught at startup.
func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("egress-proxy: invalid CIDR " + s + ": " + err.Error())
	}
	return n
}

// nat64Prefix is the well-known NAT64 translation prefix (RFC 6052). Addresses in
// it embed an IPv4 address in their low 32 bits, so they are judged by that
// address rather than blocked outright — an IPv6-only cluster reaches the public
// IPv4 internet through exactly this prefix.
var nat64Prefix = mustCIDR("64:ff9b::/96")

// disallowedCIDRs holds the internal ranges Go's net.IP predicates do not already
// cover. Together with those predicates this mirrors the deny list of the chart's
// kata egress NetworkPolicy (infra/helm/codearmory/templates/forge-egress-networkpolicy-kata.yaml)
// so the platform's two egress boundaries agree on what "public" means.
var disallowedCIDRs = []*net.IPNet{
	// CGNAT. Clusters commonly place the pod/node network here (EKS, GKE, OKE),
	// and Alibaba Cloud serves instance metadata from 100.100.100.200.
	mustCIDR("100.64.0.0/10"),
	// "This network" — only 0.0.0.0 itself is IsUnspecified, but the whole /8 is
	// non-routable and is treated as the local host by many stacks.
	mustCIDR("0.0.0.0/8"),
	// Reserved / future use, including the 255.255.255.255 broadcast address.
	mustCIDR("240.0.0.0/4"),
	// Deprecated IPv6 site-local. (Its replacement, unique-local fc00::/7, is
	// already covered by IsPrivate.)
	mustCIDR("fec0::/10"),
}

// isDisallowedIP reports whether ip is one the proxy must never dial: loopback,
// private (RFC1918 and IPv6 unique-local fc00::/7), link-local (covers the
// 169.254.169.254 cloud-metadata endpoint), multicast, unspecified, or any range
// in disallowedCIDRs. The domain allowlist only matches on hostname, so without
// this an allowlisted (or attacker-DNS-controlled) name resolving to an internal
// address would let sandboxed code reach metadata / cluster services.
func isDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// Normalize an IPv4-mapped IPv6 address (::ffff:10.0.0.1) to its 4-byte form
	// so the IPv4 ranges cannot be evaded by expressing the address as IPv6.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else if ip16 := ip.To16(); ip16 != nil && nat64Prefix.Contains(ip16) {
		// A NAT64 address reaches the IPv4 address in its low 32 bits, so judge it
		// by that: 64:ff9b::a00:1 must be blocked exactly like 10.0.0.1.
		return isDisallowedIP(net.IP(ip16[12:16]))
	}
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	for _, n := range disallowedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// allowDialIP reports whether the proxy may dial ip. Overridable in tests that
// must reach a loopback httptest server; production always uses the strict check.
var allowDialIP = func(ip net.IP) bool { return !isDisallowedIP(ip) }

// safeDialContext resolves addr and dials only an IP that passes allowDialIP.
// Validating at dial time (rather than trusting an earlier lookup) also defeats
// DNS-rebinding TOCTOU: the address we connect to is the one we checked.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	lastErr := fmt.Errorf("no permitted address for %q", host)
	for _, ip := range ips {
		if !allowDialIP(ip.IP) {
			slog.WarnContext(ctx, "egress blocked: host resolved to non-public address", "host", host, "resolved_ip", ip.IP.String())
			lastErr = fmt.Errorf("egress to non-public address %s blocked", ip.IP)
			continue
		}
		conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, lastErr
}

type proxy struct{ al allowlist }

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health check — direct request to the proxy itself, not a forwarded request.
	if r.Method == http.MethodGet && !r.URL.IsAbs() && r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}

	if !r.URL.IsAbs() {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	p.handleHTTP(w, r)
}

func (p *proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("egress-proxy").Start(r.Context(), "egress.connect")
	defer span.End()

	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	span.SetAttributes(
		attribute.String("egress.method", "CONNECT"),
		attribute.String("server.address", host),
	)
	if !p.al.permits(host) {
		span.SetAttributes(attribute.Bool("egress.allowed", false))
		span.SetStatus(codes.Error, "host not in allowlist")
		slog.WarnContext(ctx, "blocked", "host", host, "method", "CONNECT")
		http.Error(w, "forbidden: "+host, http.StatusForbidden)
		return
	}
	span.SetAttributes(attribute.Bool("egress.allowed", true))
	slog.DebugContext(ctx, "allowed", "host", host, "method", "CONNECT")

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	target, err := safeDialContext(dialCtx, "tcp", r.Host)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "dial failed")
		http.Error(w, "dial failed", http.StatusBadGateway)
		return
	}
	defer target.Close()

	hijack, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijack.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	span.SetStatus(codes.Ok, "")

	// Copy in both directions concurrently. Each goroutine signals done when the
	// connection half-closes; we wait for both so neither side is closed early.
	done := make(chan struct{}, 2)
	go func() { io.Copy(target, clientConn); done <- struct{}{} }() //nolint:errcheck
	go func() { io.Copy(clientConn, target); done <- struct{}{} }() //nolint:errcheck
	<-done
	<-done
}

func (p *proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("egress-proxy").Start(r.Context(), "egress.http")
	defer span.End()

	host, _, err := net.SplitHostPort(r.URL.Host)
	if err != nil {
		host = r.URL.Host
	}
	span.SetAttributes(
		attribute.String("egress.method", r.Method),
		attribute.String("server.address", host),
	)
	if !p.al.permits(host) {
		span.SetAttributes(attribute.Bool("egress.allowed", false))
		span.SetStatus(codes.Error, "host not in allowlist")
		slog.WarnContext(ctx, "blocked", "host", host, "method", r.Method)
		http.Error(w, "forbidden: "+host, http.StatusForbidden)
		return
	}
	span.SetAttributes(attribute.Bool("egress.allowed", true))
	slog.DebugContext(ctx, "allowed", "host", host, "method", r.Method)

	out, err := http.NewRequestWithContext(ctx, r.Method, r.URL.String(), r.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "bad request")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for k, vv := range r.Header {
		if !hopByHop[strings.ToLower(k)] {
			out.Header[k] = vv
		}
	}

	resp, err := fwdClient.Do(out)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "upstream error")
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	span.SetStatus(codes.Ok, "")

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
}

func main() {
	logLevel := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		_ = logLevel.UnmarshalText([]byte(v))
	}
	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(jsonHandler))

	otelHandler, shutdown, err := telemetry.Setup(context.Background(), "egress-proxy")
	if err != nil {
		slog.Warn("OpenTelemetry setup failed, logging to stderr only", "error", err)
	} else {
		slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
		defer shutdown(context.Background())
	}

	al := parseAllowlist(os.Getenv("PROXY_ALLOWED_DOMAINS"))
	switch {
	case len(al) == 0:
		slog.Warn("PROXY_ALLOWED_DOMAINS is not set — all proxy requests will be blocked")
	case al.allowsAll():
		slog.Warn("PROXY_ALLOWED_DOMAINS=* — public-only mode: any PUBLIC host is allowed; private/internal/metadata addresses stay blocked by the IP guard")
	}

	port := "3128"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           &proxy{al: al},
		ReadHeaderTimeout: 10 * time.Second,
	}

	slog.Info("egress proxy listening", "port", port, "allowed_domains", len(al), "public_only", al.allowsAll())

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, os.Interrupt)
	<-sigCh
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx) //nolint:errcheck
}
