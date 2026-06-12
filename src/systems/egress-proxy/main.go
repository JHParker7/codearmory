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
	"strings"
	"syscall"
	"time"
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

// isDisallowedIP reports whether ip is one the proxy must never dial: loopback,
// private, link-local (covers the 169.254.169.254 cloud-metadata endpoint),
// multicast, or unspecified. The domain allowlist only matches on hostname, so
// without this an allowlisted (or attacker-DNS-controlled) name resolving to an
// internal address would let sandboxed code reach metadata / cluster services.
func isDisallowedIP(ip net.IP) bool {
	return ip == nil ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
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
			slog.Warn("egress blocked: host resolved to non-public address", "host", host, "resolved_ip", ip.IP.String())
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
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if !p.al.permits(host) {
		slog.Info("blocked", "host", host, "method", "CONNECT")
		http.Error(w, "forbidden: "+host, http.StatusForbidden)
		return
	}
	slog.Info("allowed", "host", host, "method", "CONNECT")

	dialCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	target, err := safeDialContext(dialCtx, "tcp", r.Host)
	if err != nil {
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

	// Copy in both directions concurrently. Each goroutine signals done when the
	// connection half-closes; we wait for both so neither side is closed early.
	done := make(chan struct{}, 2)
	go func() { io.Copy(target, clientConn); done <- struct{}{} }()    //nolint:errcheck
	go func() { io.Copy(clientConn, target); done <- struct{}{} }()    //nolint:errcheck
	<-done
	<-done
}

func (p *proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.URL.Host)
	if err != nil {
		host = r.URL.Host
	}
	if !p.al.permits(host) {
		slog.Info("blocked", "host", host, "method", r.Method)
		http.Error(w, "forbidden: "+host, http.StatusForbidden)
		return
	}
	slog.Info("allowed", "host", host, "method", r.Method)

	out, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
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
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	al := parseAllowlist(os.Getenv("PROXY_ALLOWED_DOMAINS"))
	if len(al) == 0 {
		slog.Warn("PROXY_ALLOWED_DOMAINS is not set — all proxy requests will be blocked")
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

	slog.Info("egress proxy listening", "port", port, "allowed_domains", len(al))

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
