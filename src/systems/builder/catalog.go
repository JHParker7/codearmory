package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
)

// catalogEntry is a single service as advertised by the registry.
type catalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// registryURL / getRegistryKey are set in main(). getRegistryKey returns the
// current rotating service key used to authenticate builder's calls to the
// registry (it is registered as a read service account there).
var (
	registryURL    string
	getRegistryKey func() string
)

// catalog cache: the registry advertises every platform service. Toggling is a
// rare, admin-facing action, so a short in-memory cache keeps the list fresh
// without hammering the registry on every screen open.
var (
	catalogMu       sync.Mutex
	catalogCached   []catalogEntry
	catalogFetched  time.Time
	catalogCacheTTL = 5 * time.Minute
)

// serviceCatalog returns the list of toggle-able platform services (core services
// excluded). The builder-embedded definitions are the authoritative set and are
// always present (even when the registry is unreachable); registry-live non-core
// services (custom/out-of-band registrations) are merged on top.
func serviceCatalog(ctx context.Context) []catalogEntry {
	byName := map[string]catalogEntry{}
	if defs, err := loadEmbeddedDefs(); err == nil {
		for _, d := range defs {
			byName[d.RegistryName] = catalogEntry{Name: d.RegistryName, Description: d.Description}
		}
	}
	for _, e := range liveCatalog(ctx) {
		if _, ok := byName[e.Name]; !ok {
			byName[e.Name] = e
		}
	}
	out := make([]catalogEntry, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// liveCatalog returns the registry's live non-core services. It is best-effort: on a
// registry error it returns the last good cache (possibly empty) so the admin UI
// degrades to the embedded/configured rows rather than failing outright.
func liveCatalog(ctx context.Context) []catalogEntry {
	ctx, span := otel.Tracer("builder").Start(ctx, "catalog.list")
	defer span.End()

	catalogMu.Lock()
	// Gate on whether a fetch has happened, not on catalogCached != nil: an
	// empty-but-successfully-fetched catalog must still honor the TTL instead of
	// re-hitting the registry on every admin list call.
	if !catalogFetched.IsZero() && time.Since(catalogFetched) < catalogCacheTTL {
		out := catalogCached
		catalogMu.Unlock()
		return out
	}
	catalogMu.Unlock()

	fresh, err := fetchCatalog(ctx)
	if err != nil {
		slog.WarnContext(ctx, "service catalog fetch failed; using cached/partial list", "error", err)
		catalogMu.Lock()
		defer catalogMu.Unlock()
		return catalogCached
	}

	catalogMu.Lock()
	catalogCached = fresh
	catalogFetched = time.Now()
	catalogMu.Unlock()
	return fresh
}

// liveServiceNames returns the set of non-core service names the registry currently
// advertises. The registry is the source of truth for what conductor will route, so
// builder uses it to mark a service live in the effective view even when builder has
// no baseline row for it — e.g. forge/workflows, which the chart ships and registers
// directly. Best-effort: an empty set on a registry blip degrades a live service to
// default-OFF rather than failing the list.
func liveServiceNames(ctx context.Context) map[string]bool {
	set := map[string]bool{}
	for _, e := range liveCatalog(ctx) {
		set[e.Name] = true
	}
	return set
}

func fetchCatalog(ctx context.Context) ([]catalogEntry, error) {
	if registryURL == "" || getRegistryKey == nil {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL+"/services", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Service-Key", "builder:"+getRegistryKey())

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		// Return an error (not nil,nil) so serviceCatalog keeps the last-good
		// cache instead of clobbering it with an empty list during a registry blip.
		return nil, fmt.Errorf("registry /services returned %d", resp.StatusCode)
	}
	var svcs []catalogEntry
	if err := json.NewDecoder(resp.Body).Decode(&svcs); err != nil {
		return nil, err
	}
	out := make([]catalogEntry, 0, len(svcs))
	for _, s := range svcs {
		if coreServices[s.Name] {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}
