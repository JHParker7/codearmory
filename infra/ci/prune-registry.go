// prune-registry keeps a bounded number of image tags per package in the Forgejo
// container registry, deleting the oldest beyond that, and reports the disk it can
// see but cannot reclaim.
//
// Why this exists: codearmory-ci pushes every service image at the commit SHA, and
// nothing ever removed them. On the dev VM that reached 34 tags of jp01/portal
// alone — 190 images, ~52GB — and the root filesystem hit 100% mid-session, at
// which point builds fail in ways that look like anything but a full disk. The
// second failure mode is worse: kaniko does not fail fast on a full disk, it
// crawls — one build-push took 27 minutes against a normal 90 seconds and died on
// "context deadline exceeded", which reads as a network or registry problem.
//
// Why SHA tags are kept rather than replaced by a single mutable :experimental tag:
// the pipeline redeploys with `outpost set-image`, which is a no-op when the tag
// string does not change. A fixed tag needs BOTH an explicit rollout command (the
// outpost deploy module has one) AND imagePullPolicy: Always on every deployment,
// or the node keeps serving its cached copy of the previous build. That second half
// lives in the Helm chart, not here, so a tag change made alone would silently stop
// deploying anything — a worse failure than a full disk, because it is invisible.
// Retention keeps the deploy path working and bounds the growth instead.
//
// Two retention rules, applied per image, whichever deletes more:
//   - -keep N: only the newest N tags survive. This is the hard bound on growth.
//   - -max-age D: tags older than D go too, down to a floor of -keep-min. This is
//     what stops a service that was rebuilt 5 times last month from holding 5 stale
//     images forever.
//
// What it deliberately does NOT do: reclaim the node's containerd image cache, the
// hostpath PVCs behind forge's volumes, or the host's own docker images and volumes.
// None of those are reachable from a forge sandbox — see reportUnreachable. It says
// so loudly rather than pretending, and prints the disk pressure it can measure so a
// slow build has an explanation attached to it in the run log.
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

// retention is the policy prunable applies. Zero maxAge disables the age rule, so
// the struct's zero value plus a keep is the old count-only behaviour.
type retention struct {
	keep      int           // newest N per image always survive
	keepMin   int           // floor the age rule will not prune below
	maxAge    time.Duration // 0 disables age-based pruning
	now       time.Time     // injected so the age rule is testable
	protected map[string]bool
}

// prunable decides, per package, which versions to delete. A version goes when it
// falls outside the newest `keep`, OR when it is older than `maxAge` and is not one
// of the newest `keepMin`. Protected tags are never candidates.
//
// The keepMin floor matters: without it a service that stopped being touched would
// have every tag age out, and the tag its running deployment references would be
// deleted out from under it — harmless until a pod reschedules, then ImagePullBackOff.
// keepMin >= 1 guarantees the newest tag (which is what is deployed, since the
// pipeline deploys what it just built) always survives.
//
// Ordering is newest-first by CreatedAt, with the version string as a tiebreak so
// the result is deterministic when timestamps collide (they do — a run pushes
// several services within the same second).
func prunable(versions []packageVersion, r retention) map[string][]packageVersion {
	byName := map[string][]packageVersion{}
	for _, v := range versions {
		if r.protected[v.Version] {
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
		var doomed []packageVersion
		for i, v := range vs {
			tooMany := i >= r.keep
			tooOld := r.maxAge > 0 && i >= r.keepMin && r.now.Sub(v.CreatedAt) > r.maxAge
			if tooMany || tooOld {
				doomed = append(doomed, v)
			}
		}
		if len(doomed) > 0 {
			out[name] = doomed
		}
	}
	return out
}

// humanBytes formats a byte count for a human reading a CI log.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTP"[exp])
}

// reportDisk prints usage for each path and returns the worst used-percentage seen,
// or -1 when nothing could be measured.
//
// This is the only view of node disk pressure the pipeline has. A forge sandbox's
// "/" is the node's filesystem (the pod's writable layer lives on it), so a full
// node shows up here even though nothing in this process can clean it.
func reportDisk(paths []string, warnPct int) int {
	worst := -1
	fmt.Println("=== disk ===")
	for _, p := range paths {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		total, avail, err := diskUsage(p)
		if err != nil {
			fmt.Printf("  %-14s unavailable: %v\n", p, err)
			continue
		}
		if total == 0 {
			continue
		}
		used := total - avail
		pct := int(used * 100 / total)
		flag := ""
		if pct >= warnPct {
			flag = "  <-- OVER " + fmt.Sprint(warnPct) + "%"
		}
		fmt.Printf("  %-14s %s used of %s (%d%% full, %s free)%s\n", p, humanBytes(used), humanBytes(total), pct, humanBytes(avail), flag)
		if pct > worst {
			worst = pct
		}
	}
	return worst
}

// reportUnreachable states, in the run log, what is consuming disk that this step
// cannot free. Printed loudly under pressure and as a one-liner otherwise.
//
// Being explicit is the point of this function. The alternative — a cleanup step
// that quietly no-ops against the node — is how "prune ran, disk still full" turns
// into an hour of confusion. Everything listed here needs either root on the node or
// a component that owns the data; a sandboxed pipeline step has neither.
func reportUnreachable(pressured bool) {
	if !pressured {
		fmt.Println("note: registry tags are the only disk this step can reclaim; the node's " +
			"containerd cache, forge's hostpath PVCs and the host's docker images/volumes are not reachable from a sandbox.")
		return
	}
	fmt.Println("=== NOT RECLAIMABLE FROM THIS STEP ===")
	fmt.Println("  Disk is above the warning threshold and the registry prune above is all this")
	fmt.Println("  step can do. Expect slow or timing-out image builds until one of these is")
	fmt.Println("  dealt with by hand on the host (each needs root on the node, which a forge")
	fmt.Println("  sandbox does not have):")
	fmt.Println("    - node containerd image cache (/var/lib/containerd, ~21G observed)")
	fmt.Println("        minikube ssh -- sudo crictl rmi --prune")
	fmt.Println("        NB: this itself fails with DeadlineExceeded once the disk is truly full,")
	fmt.Println("        because the removals need space to complete. Reclaim before that point.")
	fmt.Println("    - forge volume PVCs on hostpath (gocache/gitcache/npmcache + per-run, ~13G)")
	fmt.Println("        owned by forge's reaper: FORGE_VOLUME_MAX_AGE_SECS on the forge deployment")
	fmt.Println("    - the host's own docker images and volumes (~18G images, ~42G volumes,")
	fmt.Println("      of which the minikube volume alone was ~38G)")
	fmt.Println("        docker system prune -a --volumes   (on the host, not in the cluster)")
	fmt.Println("  Deleting registry tags only shrinks the Forgejo package PVC, and only after")
	fmt.Println("  Forgejo drops the now-unreferenced blobs.")
}

func main() {
	var (
		registry   = flag.String("registry", envOr("REGISTRY_HOST", "192.168.53.171:3000"), "registry host:port, also the Forgejo API host")
		scheme     = flag.String("scheme", envOr("REGISTRY_SCHEME", "http"), "http or https")
		owner      = flag.String("owner", envOr("REGISTRY_OWNER", "jp01"), "package owner")
		keep       = flag.Int("keep", envIntOr("KEEP_TAGS", 10), "how many of the newest tags to keep per image")
		keepMin    = flag.Int("keep-min", envIntOr("KEEP_MIN_TAGS", 2), "tags per image the -max-age rule will never prune below")
		maxAge     = flag.Duration("max-age", envDurationOr("MAX_TAG_AGE", 0), "also delete tags older than this, down to -keep-min (0 disables)")
		apply      = flag.Bool("apply", false, "actually delete; without it, only report")
		strict     = flag.Bool("strict", false, "exit non-zero when pruning fails")
		keepTagCSV = flag.String("keep-tags", os.Getenv("PROTECTED_TAGS"), "comma-separated tags never deleted (in addition to 'latest')")
		diskPaths  = flag.String("disk", envOr("DISK_PATHS", "/"), "comma-separated paths to report free space for")
		warnPct    = flag.Int("disk-warn-pct", envIntOr("DISK_WARN_PCT", 85), "used-percentage at which to print the not-reclaimable report")
	)
	flag.Parse()

	// Measured first, and before anything that can bail out: when the registry API
	// is unreachable because the box is wedged on a full disk, the disk numbers are
	// the single most useful line in the log and must not be lost with the error.
	worstPct := reportDisk(strings.Split(*diskPaths, ","), *warnPct)
	pressured := worstPct >= *warnPct

	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "prune-registry: "+format+"\n", args...)
		reportUnreachable(pressured)
		if *strict {
			os.Exit(1)
		}
		os.Exit(0)
	}

	if *keep < 1 {
		fail("keep must be at least 1, got %d", *keep)
	}
	if *keepMin < 1 {
		fail("keep-min must be at least 1, got %d", *keepMin)
	}
	if *keepMin > *keep {
		// Otherwise the age rule could delete inside the window -keep promises to
		// hold, which makes the two flags contradict each other.
		fail("keep-min (%d) must not exceed keep (%d)", *keepMin, *keep)
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
	doomed := prunable(versions, retention{
		keep:      *keep,
		keepMin:   *keepMin,
		maxAge:    *maxAge,
		now:       time.Now(),
		protected: protected,
	})

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
	age := "off"
	if *maxAge > 0 {
		age = fmt.Sprintf("%s (floor %d)", *maxAge, *keepMin)
	}
	fmt.Printf("prune-registry: %d versions across %d images; keeping newest %d per image, max-age %s; %s %d (%d failed)\n",
		len(versions), countImages(versions), *keep, age, verb, total, failed)
	if failed > 0 {
		// Never silent: a prune that has been quietly failing for a week is
		// indistinguishable from one that was never wired up, until the disk fills.
		fmt.Fprintf(os.Stderr, "prune-registry: WARNING %d deletions failed — registry growth is NOT bounded until this is fixed\n", failed)
	}
	reportUnreachable(pressured)
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

// envDurationOr reads a Go duration ("168h", "30m"). An unparseable value falls back
// to the default rather than erroring: this is housekeeping configuration, and a
// typo in an env var must not be the reason a pipeline stops pruning.
func envDurationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return def
	}
	return d
}
