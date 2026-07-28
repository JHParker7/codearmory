// prune-registry keeps a bounded number of image tags per package in the Forgejo
// container registry, deleting the oldest beyond that.
//
// Why this exists: codearmory-ci pushes every service image at the commit SHA, and
// nothing ever removed them. On the dev VM that reached 34 tags of jp01/portal
// alone — 190 images, ~52GB — and the root filesystem hit 100% mid-session, at
// which point builds fail in ways that look like anything but a full disk.
//
// Why SHA tags are kept rather than replaced by a single mutable :experimental tag:
// the pipeline redeploys with `outpost set-image`, which is a no-op when the tag
// string does not change. A fixed tag would leave every rollout silently doing
// nothing unless an explicit restart were added and imagePullPolicy forced to
// Always. Retention keeps the deploy path working and bounds the growth instead.
//
// Run with no arguments to see what would be deleted; -apply performs the deletes.
// Failures are reported but never fatal (exit 0 unless -strict): housekeeping must
// not turn a green pipeline red.
//
// Standard library only, in its own dependency-free module, so `go run ./infra/ci`
// runs it in CI with no module download.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// packageVersion is one version (for a container package, one tag) as the Forgejo
// packages API reports it.
type packageVersion struct {
	Type      string    `json:"type"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// dockerConfig is the subset of a docker config.json we need. The pipeline already
// holds one as the forgejo-registry-auth secret for kaniko, so pruning needs no
// second credential.
type dockerConfig struct {
	Auths map[string]struct {
		Auth     string `json:"auth"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auths"`
}

// credentials pulls a username/password for host out of a docker config.json.
// Falls back to the sole entry when no key matches, since a config written for
// "host:port" is sometimes keyed by bare host (or by an https:// URL).
func credentials(raw, host string) (string, string, error) {
	var cfg dockerConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return "", "", fmt.Errorf("REGISTRY_AUTH is not a docker config.json: %w", err)
	}
	if len(cfg.Auths) == 0 {
		return "", "", fmt.Errorf("REGISTRY_AUTH has no auths entries")
	}
	entry, ok := cfg.Auths[host]
	if !ok {
		for k, v := range cfg.Auths {
			if strings.Contains(k, host) || strings.Contains(host, strings.TrimPrefix(strings.TrimPrefix(k, "https://"), "http://")) {
				entry, ok = v, true
				break
			}
		}
	}
	if !ok && len(cfg.Auths) == 1 {
		for _, v := range cfg.Auths {
			entry, ok = v, true
		}
	}
	if !ok {
		return "", "", fmt.Errorf("REGISTRY_AUTH has no credentials for %s", host)
	}
	if entry.Username != "" && entry.Password != "" {
		return entry.Username, entry.Password, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return "", "", fmt.Errorf("auth for %s is not valid base64: %w", host, err)
	}
	user, pass, found := strings.Cut(string(decoded), ":")
	if !found {
		return "", "", fmt.Errorf("auth for %s is not user:password", host)
	}
	return user, pass, nil
}

type client struct {
	base       string
	owner      string
	user, pass string
	http       *http.Client
}

func (c *client) do(method, path string) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Accept", "application/json")
	return c.http.Do(req)
}

// listVersions pages through every container package version owned by c.owner.
func (c *client) listVersions() ([]packageVersion, error) {
	var all []packageVersion
	for page := 1; page <= 100; page++ { // bounded: never loop forever on a odd API
		resp, err := c.do("GET", fmt.Sprintf("/api/v1/packages/%s?type=container&page=%d&limit=50", url.PathEscape(c.owner), page))
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list packages: %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		var batch []packageVersion
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("list packages: decode: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		if len(batch) < 50 {
			break
		}
	}
	return all, nil
}

func (c *client) deleteVersion(name, version string) error {
	resp, err := c.do("DELETE", fmt.Sprintf("/api/v1/packages/%s/container/%s/%s",
		url.PathEscape(c.owner), url.PathEscape(name), url.PathEscape(version)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// 404 means someone else already removed it — the desired state either way.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// prunable decides, per package, which versions to delete: everything after the
// newest `keep` by creation time, excluding any protected tag.
//
// Ordering is newest-first by CreatedAt, with the version string as a tiebreak so
// the result is deterministic when timestamps collide (they do — a run pushes
// several services within the same second).
func prunable(versions []packageVersion, keep int, protected map[string]bool) map[string][]packageVersion {
	byName := map[string][]packageVersion{}
	for _, v := range versions {
		if protected[v.Version] {
			continue
		}
		byName[v.Name] = append(byName[v.Name], v)
	}
	out := map[string][]packageVersion{}
	for name, vs := range byName {
		sort.Slice(vs, func(i, j int) bool {
			if !vs[i].CreatedAt.Equal(vs[j].CreatedAt) {
				return vs[i].CreatedAt.After(vs[j].CreatedAt)
			}
			return vs[i].Version > vs[j].Version
		})
		if len(vs) > keep {
			out[name] = vs[keep:]
		}
	}
	return out
}

func main() {
	var (
		registry   = flag.String("registry", envOr("REGISTRY_HOST", "192.168.53.171:3000"), "registry host:port, also the Forgejo API host")
		scheme     = flag.String("scheme", envOr("REGISTRY_SCHEME", "http"), "http or https")
		owner      = flag.String("owner", envOr("REGISTRY_OWNER", "jp01"), "package owner")
		keep       = flag.Int("keep", envIntOr("KEEP_TAGS", 10), "how many of the newest tags to keep per image")
		apply      = flag.Bool("apply", false, "actually delete; without it, only report")
		strict     = flag.Bool("strict", false, "exit non-zero when pruning fails")
		keepTagCSV = flag.String("keep-tags", os.Getenv("PROTECTED_TAGS"), "comma-separated tags never deleted (in addition to 'latest')")
	)
	flag.Parse()

	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "prune-registry: "+format+"\n", args...)
		if *strict {
			os.Exit(1)
		}
		os.Exit(0)
	}

	if *keep < 1 {
		fail("keep must be at least 1, got %d", *keep)
	}
	raw := os.Getenv("REGISTRY_AUTH")
	if raw == "" {
		fail("REGISTRY_AUTH is not set")
	}
	user, pass, err := credentials(raw, *registry)
	if err != nil {
		fail("%v", err)
	}

	// 'latest' is a moving pointer other things resolve (the e2e image), never a
	// build artefact this pipeline can safely reclaim. The current run's SHA is
	// protected explicitly as well: keep-newest-N should already cover it, but a
	// clock skew between registry nodes must not delete the tag just deployed.
	protected := map[string]bool{"latest": true}
	for _, t := range strings.Split(*keepTagCSV, ",") {
		if t = strings.TrimSpace(t); t != "" {
			protected[t] = true
		}
	}

	c := &client{
		base:  *scheme + "://" + *registry,
		owner: *owner,
		user:  user, pass: pass,
		http: &http.Client{Timeout: 30 * time.Second},
	}

	versions, err := c.listVersions()
	if err != nil {
		fail("%v", err)
	}
	doomed := prunable(versions, *keep, protected)

	names := make([]string, 0, len(doomed))
	for n := range doomed {
		names = append(names, n)
	}
	sort.Strings(names)

	total, failed := 0, 0
	for _, name := range names {
		for _, v := range doomed[name] {
			total++
			if !*apply {
				fmt.Printf("would delete %s/%s:%s (%s)\n", *owner, name, v.Version, v.CreatedAt.Format(time.RFC3339))
				continue
			}
			if err := c.deleteVersion(name, v.Version); err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "delete %s/%s:%s: %v\n", *owner, name, v.Version, err)
				continue
			}
			fmt.Printf("deleted %s/%s:%s (%s)\n", *owner, name, v.Version, v.CreatedAt.Format(time.RFC3339))
		}
	}

	verb := "would delete"
	if *apply {
		verb = "deleted"
	}
	fmt.Printf("prune-registry: %d versions across %d images; keeping newest %d per image; %s %d (%d failed)\n",
		len(versions), countImages(versions), *keep, verb, total, failed)
	if failed > 0 && *strict {
		os.Exit(1)
	}
}

func countImages(vs []packageVersion) int {
	seen := map[string]bool{}
	for _, v := range vs {
		seen[v.Name] = true
	}
	return len(seen)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 1 {
		return def
	}
	return n
}
