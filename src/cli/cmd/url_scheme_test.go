package cmd

import "testing"

// TestNormalizeConductorURL pins the scheme inference. The rule is not "always https":
// a single-label host is an in-cluster service name that will never have a public
// certificate, and loopback is a dev server — defaulting either to https produces a TLS
// error that reads as the server being broken rather than the URL being wrong.
func TestNormalizeConductorURL(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// Explicit scheme is never touched, including a non-default port or path.
		{"http://conductor:8080", "http://conductor:8080"},
		{"https://exp.codearmory.app/api", "https://exp.codearmory.app/api"},
		// Trailing slashes are trimmed so the base never doubles up when a path is joined.
		{"https://exp.codearmory.app/api/", "https://exp.codearmory.app/api"},
		// Dotted, routable-looking hosts get https.
		{"exp.codearmory.app/api", "https://exp.codearmory.app/api"},
		{"exp.codearmory.app:8443", "https://exp.codearmory.app:8443"},
		// Loopback is a dev server: http.
		{"localhost:8080", "http://localhost:8080"},
		{"127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"[::1]:8080", "http://[::1]:8080"},
		// A single label is a compose/Kubernetes service name: http.
		{"conductor:8080", "http://conductor:8080"},
		{"conductor", "http://conductor"},
		// Empty stays empty — callers report "URL is required" rather than a bare scheme.
		{"", ""},
		{"   ", ""},
	} {
		if got := normalizeConductorURL(c.in); got != c.want {
			t.Errorf("normalizeConductorURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
