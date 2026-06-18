package main

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// initWebhookClient builds the HTTP client used to deliver to user-supplied
// webhook and Slack URLs. Unlike the shared httpClient — which talks only to
// trusted internal services such as gatekeeper, and so must be allowed to reach
// private addresses — this client treats its target as attacker-controlled and
// refuses to connect to any non-public IP. That blocks SSRF to loopback, private
// networks, and the cloud metadata endpoint (169.254.169.254). The check runs in
// the dialer Control hook against the *resolved* IP, so it also defeats DNS
// rebinding and redirects to internal targets — every hop re-dials and is
// re-validated.
func initWebhookClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: ssrfGuardControl}
	return &http.Client{
		Transport: otelhttp.NewTransport(&http.Transport{
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		}),
		Timeout: 15 * time.Second,
	}
}

// ssrfGuardControl rejects a dial to any non-public IP. The dialer invokes it
// after DNS resolution with the concrete IP:port about to be connected, so a
// hostname that resolves (or rebinds) to an internal address is still blocked.
func ssrfGuardControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("refusing to dial unresolved address %q", address)
	}
	if isBlockedIP(ip) {
		return fmt.Errorf("refusing to connect to non-public address %s", ip)
	}
	return nil
}

// isBlockedIP reports whether ip is in a range a user-supplied webhook URL must
// not reach: loopback, RFC1918/ULA private, link-local (including the cloud
// metadata endpoint 169.254.169.254), multicast, or the unspecified address.
func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}
