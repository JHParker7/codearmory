package main

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// The remote-node reverse proxy (ARCHITECTURE §5 Step 3). When a repo's shard resolves
// to another git-node, the entry control-plane node forwards the git wire request there
// and streams the bytes back — Smart HTTP is already HTTP, so this is a pass-through
// (§5's whole argument for a proxy over gRPC).
//
// Auth happens ONCE, at the entry node. The forwarded request carries a shared-key
// header the receiving node trusts (nodeForwardKey), so the node serves straight from
// disk without a second gatekeeper round-trip — and, since a trusted-forward request is
// never itself re-proxied, there is no forwarding loop.

const forwardHeader = "X-Git-Node-Forward"

// nodeForwardKey is the shared secret peer git-nodes use to trust a forwarded wire
// request. Empty means node-to-node forwarding is not configured: a request is never
// trusted as forwarded, and (a single-node install) nothing is ever proxied anyway.
func nodeForwardKey() string { return secret("GIT_NODE_FORWARD_KEY") }

// isTrustedForward reports whether r is a wire request forwarded by a peer node with the
// correct shared key. Such a request has already been authorized upstream.
func isTrustedForward(r *http.Request) bool {
	want := nodeForwardKey()
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(forwardHeader)), []byte(want)) == 1
}

// maybeProxyToNode forwards a wire request to the node that should serve it and returns
// true when it did (the caller must then stop). WRITES (receive-pack) go to the shard's
// primary; READS (upload-pack) fan out to a caught-up replica when one exists, else the
// primary (ARCHITECTURE §5 Step 4). It returns false — serve locally — when the chosen
// node is this one, placement can't be resolved, or this request is itself a trusted
// forward (which must be served here, never bounced onward, so there is no loop).
func maybeProxyToNode(w http.ResponseWriter, r *http.Request, re Repo, svc gitService) bool {
	if isTrustedForward(r) {
		return false
	}
	var (
		node nodeRef
		err  error
	)
	if svc == svcReceivePack {
		node, err = resolveNode(r.Context(), re.ID, "") // writes → authoritative primary
	} else {
		node, err = pickReadNode(r.Context(), re.ID, re.Version) // reads → caught-up replica/primary
	}
	if err != nil || node.Local || node.Address == "" {
		return false
	}
	slog.InfoContext(r.Context(), "git proxy: forwarding", "repo_id", re.ID, "service", svc, "node", node.Address)
	// A clone/push streams for far longer than the server's write timeout; clear the
	// deadlines before handing the connection to the proxy.
	unboundedStream(w, r)
	proxyToNode(w, r, node.Address)
	return true
}

// proxyToNode streams r to the git-node at addr (its base URL), preserving the wire path
// and adding the trust header. On a dial/transport failure it answers 502 — the bytes
// are on a node we could not reach, which is a gateway error, not the client's fault.
func proxyToNode(w http.ResponseWriter, r *http.Request, addr string) {
	target, err := url.Parse(addr)
	if err != nil {
		slog.ErrorContext(r.Context(), "git proxy: bad node address", "address", addr, "error", err)
		http.Error(w, "bad node address", http.StatusBadGateway)
		return
	}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Keep the same path/query — the node exposes the identical wire routes.
			req.Header.Set(forwardHeader, nodeForwardKey())
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			slog.ErrorContext(req.Context(), "git proxy: forward failed", "address", addr, "error", err)
			http.Error(rw, "git node unreachable", http.StatusBadGateway)
		},
		// FlushInterval -1 flushes each write immediately, which pkt-line negotiation
		// depends on — buffering would deadlock the client waiting for a response it
		// can't see yet.
		FlushInterval: -1,
	}
	proxy.ServeHTTP(w, r)
}
