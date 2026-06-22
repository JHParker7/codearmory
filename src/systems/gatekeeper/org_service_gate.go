package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
)

// The org-service disable-gate denies requests to services an org has turned off
// in the builder service. The desired state lives in builder; gatekeeper is the
// universal choke point (every service calls /check_permissions), so the gate
// hooks into permission evaluation here.
//
// It is deliberately minimal and safe:
//   - config-gated: inert unless both BUILDER_URL and BUILDER_INTERNAL_KEY are set;
//   - fail-open: a builder outage or error never blocks (preserves default-on);
//   - cached: the per-org disabled-set is cached in Redis with a short TTL so the
//     hot path does at most one builder call per org per TTL window.
var (
	builderURL         = strings.TrimRight(os.Getenv("BUILDER_URL"), "/")
	builderInternalKey = secret("BUILDER_INTERNAL_KEY")
)

// gateCoreServices are never gated — disabling them would sever the platform.
var gateCoreServices = map[string]bool{
	"gatekeeper": true,
	"conductor":  true,
	"registry":   true,
	"builder":    true,
}

const orgServiceCacheTTL = 30 * time.Second

var builderGateClient = &http.Client{Timeout: 3 * time.Second}

// serviceDisabledForOrg reports whether `service` is explicitly disabled for the
// caller's org. Returns false (allow) whenever the gate is unconfigured, the
// service is core, or builder cannot be reached.
func serviceDisabledForOrg(ctx context.Context, orgID, service string) bool {
	if builderURL == "" || builderInternalKey == "" {
		return false
	}
	if gateCoreServices[service] {
		return false
	}
	if orgID == "" {
		orgID = "default" // no-org users resolve against the default baseline
	}
	return orgDisabledSet(ctx, orgID)[service]
}

// orgDisabledSet returns the cached set of disabled services for an org, fetching
// from builder on a cache miss. Fails open (empty set) on any error.
func orgDisabledSet(ctx context.Context, orgID string) map[string]bool {
	key := "orgsvc:" + orgID
	if cached, ok := cacheGet[[]string](ctx, key); ok {
		return sliceToSet(cached)
	}
	list, err := fetchDisabledFromBuilder(ctx, orgID)
	if err != nil {
		slog.WarnContext(ctx, "org-service gate: builder unreachable, failing open", "org_id", orgID, "error", err)
		return nil
	}
	cacheSet(ctx, key, list, orgServiceCacheTTL)
	return sliceToSet(list)
}

func fetchDisabledFromBuilder(ctx context.Context, orgID string) ([]string, error) {
	ctx, span := otel.Tracer("gatekeeper").Start(ctx, "orgServiceGate.fetch")
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, builderURL+"/internal/org-services/effective?org_id="+orgID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+builderInternalKey)
	resp, err := builderGateClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &builderStatusError{status: resp.StatusCode}
	}
	var body struct {
		Disabled []string `json:"disabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Disabled, nil
}

type builderStatusError struct{ status int }

func (e *builderStatusError) Error() string {
	return "builder returned status " + http.StatusText(e.status)
}

func sliceToSet(s []string) map[string]bool {
	if len(s) == 0 {
		return nil
	}
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}
