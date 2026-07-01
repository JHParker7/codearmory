package cmd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Step-action input schemas.
//
// Every workflow action consumes a flat `with` map that the workflows worker
// turns into the target service's request body — path placeholders like {id} and
// {name} are filled from same-named with keys, everything else becomes the JSON
// body, and registry body-transforms may rename keys (e.g. forge's run→command).
//
// Rather than make the user hand-write that JSON, the create-step form renders a
// tailored set of inputs per action and assembles the with map from them. Actions
// without a known schema fall back to a single free-form With JSON field, so
// custom/unknown catalog actions still work.
//
// Schema field keys are namespaced "with.<key>" (withKeyPrefix) so they never
// collide with the form's own step-level fields (name, action, timeout, desc) —
// notably argo/sync's {name} path param vs. the step's own name.

const (
	// withKeyPrefix namespaces schema field keys within the form to avoid
	// collisions with the step-level name/action/timeout/desc fields.
	withKeyPrefix = "with."
	// rawWithKey is the schema key of the optional/advanced free-form With JSON
	// field that overlays extra keys not covered by a tailored schema.
	rawWithKey = "__raw"
)

// stepFieldKind controls how a schema field's text value is parsed into the with map.
type stepFieldKind int

const (
	stepFieldText         stepFieldKind = iota // plain string
	stepFieldInt                               // non-negative integer
	stepFieldEnv                               // KEY=VALUE tokens → map[string]string
	stepFieldJSON                              // raw JSON object, merged as the with base
	stepFieldVolumeAttach                      // "name[:/mount]" → a shared-volume attach
	stepFieldList                              // comma-separated string → []string
)

// registryAuthEnv is the env var (set via a secret_ref) a build reads its registry
// Docker config.json from; the build-image form wires the chosen org secret to it.
const registryAuthEnv = "REGISTRY_AUTH"

// volumeRunIDRef scopes an attached/created shared volume to the current run: the
// workflows worker substitutes ${run_id} with the run's id at dispatch, so the
// create-volume step and every attach agree on one per-run volume. defaultMountPath
// is where an attach lands when no path is given.
const (
	volumeRunIDRef    = "${run_id}"
	defaultMountPath  = "/workspace"
	defaultVolumeName = "workspace"
)

// Field catalogs back a field with a name→value picker. ids are never typed by
// hand: the field shows human-friendly names and submits the underlying id (or,
// for images, the image ref itself). When a catalog is unavailable the field
// degrades to a free-text input so the form still works.
const (
	catNone        = ""
	catImage       = "image"        // forge image allowlist (name == value)
	catTicket      = "ticket"       // tickets: show title, submit ticket_id
	catOutpost     = "outpost"      // outposts: show name, submit outpost_id
	catRunnerClass = "runner-class" // forge runner classes (name == value)
)

// stepField describes one input within an action's schema. key is the with-map
// key it fills (without the withKeyPrefix the form adds); catalog, when set,
// renders the field as a name picker over that catalog instead of free text.
type stepField struct {
	key         string
	label       string
	placeholder string
	required    bool
	kind        stepFieldKind
	catalog     string
	multiline   bool // render as a multi-line textarea (commands, etc.)
}

// kvCatalog is a parallel list of display names and the values submitted for them
// (e.g. ticket titles → ticket_ids). It backs the form's name→id pickers.
type kvCatalog struct {
	labels []string
	values []string
}

// stepCatalogs bundles the option sources the create-step form draws on, so a
// single value threads through form construction and rebuilds.
type stepCatalogs struct {
	images        []string
	tickets       kvCatalog
	outposts      kvCatalog
	runnerClasses kvCatalog
}

// forCatalog returns the kvCatalog backing a catalog name (empty for image/none).
func (c stepCatalogs) forCatalog(name string) kvCatalog {
	switch name {
	case catTicket:
		return c.tickets
	case catOutpost:
		return c.outposts
	case catRunnerClass:
		return c.runnerClasses
	default:
		return kvCatalog{}
	}
}

// stepActionSchema maps a catalog action to its ordered, tailored field set.
// Keep these in sync with the target services' request bodies; fields not listed
// here can still be supplied via the trailing advanced With JSON field.
var stepActionSchema = map[string][]stepField{
	"forge/run": {
		{key: "image", label: "Image", placeholder: "ubuntu:22.04 (required)", required: true, catalog: catImage},
		{key: "run", label: "Run", placeholder: "go test ./...\n(multi-line ok)", required: true, multiline: true},
		{key: "env", label: "Input variables", placeholder: "REPO_URL=  BRANCH=main", kind: stepFieldEnv},
		{key: "runner_class", label: "Runner", placeholder: "runner class (optional, default standard)", catalog: catRunnerClass},
		{key: "volumes", label: "Attach volume", placeholder: "workspace or workspace:/src (optional)", kind: stepFieldVolumeAttach},
	},
	"forge/create-volume": {
		{key: "name", label: "Volume name", placeholder: "workspace (default)"},
		{key: "size_mb", label: "Size MB", placeholder: "1024 (optional)", kind: stepFieldInt},
		{key: "medium", label: "Medium", placeholder: "memory (default) | disk"},
		{key: "mount_path", label: "Mount path", placeholder: "/workspace (default)"},
	},
	"forge/build-image": {
		{key: "destinations", label: "Push to", placeholder: "reg.io/acme/app:1.0, reg.io/acme/app:latest (required)", required: true, kind: stepFieldList},
		{key: "dockerfile", label: "Dockerfile", placeholder: "Dockerfile (default, relative to context)"},
		{key: "context", label: "Context", placeholder: "/workspace (default)"},
		{key: "build_args", label: "Build args", placeholder: "VERSION=1.0 COMMIT=abc", kind: stepFieldEnv},
		{key: "target", label: "Target stage", placeholder: "multi-stage target (optional)"},
		{key: "registry_secret", label: "Registry secret", placeholder: "org secret holding a docker config.json (to push)"},
		{key: "volumes", label: "Source volume", placeholder: "workspace or workspace:/src (attach the checkout)", kind: stepFieldVolumeAttach},
		{key: "runner_class", label: "Build runner", placeholder: "privileged kata/gvisor class (required)", required: true, catalog: catRunnerClass},
	},
	"forge/git-clone": {
		{key: "volumes", label: "Clone into volume", placeholder: "workspace or workspace:/src (create with forge/create-volume)", required: true, kind: stepFieldVolumeAttach},
		{key: "image", label: "Git image", placeholder: "alpine/git (required, must include git)", required: true, catalog: catImage},
		{key: "path", label: "Clone dir", placeholder: "repo name (default, relative to the volume)"},
		{key: "ref", label: "Branch / tag", placeholder: "main (optional, default remote HEAD)"},
		{key: "depth", label: "Depth", placeholder: "1 (default shallow; 0 = full clone)", kind: stepFieldInt},
		{key: "run", label: "Post-clone command", placeholder: "true (optional; runs in the checkout after clone)"},
	},
	"tickets/create": {
		{key: "title", label: "Title", placeholder: "Build failed", required: true},
		{key: "description", label: "Desc", placeholder: "ticket body (optional)"},
		{key: "priority", label: "Priority", placeholder: "low|medium|high|critical (optional)"},
		{key: "status", label: "Status", placeholder: "open|in_progress|resolved|closed (optional)"},
	},
	"tickets/update": {
		{key: "id", label: "Ticket", placeholder: "ticket title (required)", required: true, catalog: catTicket},
		{key: "title", label: "Title", placeholder: "ticket title (required)", required: true},
		{key: "status", label: "Status", placeholder: "open|in_progress|resolved|closed (optional)"},
		{key: "priority", label: "Priority", placeholder: "low|medium|high|critical (optional)"},
		{key: "description", label: "Desc", placeholder: "ticket body (optional)"},
	},
	"tickets/delete": {
		{key: "id", label: "Ticket", placeholder: "ticket title (required)", required: true, catalog: catTicket},
	},
	"argo/sync": {
		{key: "name", label: "App", placeholder: "argo app name (required)", required: true},
		{key: "revision", label: "Revision", placeholder: "HEAD (optional)"},
		{key: "outpost_id", label: "Outpost", placeholder: "outpost name (optional)", catalog: catOutpost},
	},
	"chaos/run-experiment": {
		{key: "outpost_id", label: "Outpost", placeholder: "outpost name (required)", required: true, catalog: catOutpost},
		{key: "experiment_type", label: "Type", placeholder: "pod-delete|pod-network-latency (required)", required: true},
		{key: "target_app_ns", label: "Namespace", placeholder: "k8s namespace (required)", required: true},
		{key: "target_app_label", label: "Selector", placeholder: "app=foo label selector (required)", required: true},
		{key: "target_app_kind", label: "Kind", placeholder: "deployment (optional)"},
		{key: "params", label: "Params", placeholder: "KEY=VALUE tuning (optional)", kind: stepFieldEnv},
	},
	"blueprints/backend": {
		{key: "workspace", label: "Workspace", placeholder: "workspace name (optional)"},
		{key: "ttl_secs", label: "TTL secs", placeholder: "14400 (optional)", kind: stepFieldInt},
	},
}

// schemaKey buckets an action for change-detection: a known action keys to itself,
// every unknown/custom action keys to "" (the generic single-With-field form). The
// form only rebuilds its inputs when this bucket changes, so cycling between two
// custom actions (both generic) doesn't churn the fields.
func schemaKey(action string) string {
	if _, ok := stepActionSchema[action]; ok {
		return action
	}
	return ""
}

// schemaForAction returns the input fields for an action. Known actions get their
// tailored set plus a trailing optional advanced-With field for extra keys;
// unknown/empty actions fall back to a single required With JSON field.
func schemaForAction(action string) []stepField {
	if fields, ok := stepActionSchema[action]; ok {
		out := make([]stepField, 0, len(fields)+1)
		out = append(out, fields...)
		out = append(out, stepField{
			key:         rawWithKey,
			label:       "With",
			placeholder: `{"extra":"json"} (optional)`,
			kind:        stepFieldJSON,
		})
		return out
	}
	return []stepField{{
		key:         rawWithKey,
		label:       "With",
		placeholder: `{"key":"value"} JSON (required)`,
		required:    true,
		kind:        stepFieldJSON,
	}}
}

// buildStepWith assembles the with map from an action's schema field values,
// validating required fields. valueOf reads a form field by its prefixed key.
// Typed fields are applied first; the advanced With JSON only fills keys the typed
// fields didn't set, so it's a pure escape hatch for extra/uncommon keys.
func buildStepWith(action string, valueOf func(string) string) (map[string]any, error) {
	with := map[string]any{}
	var base map[string]any // deferred from the advanced With JSON field

	for _, f := range schemaForAction(action) {
		raw := strings.TrimSpace(valueOf(withKeyPrefix + f.key))

		if f.kind == stepFieldJSON {
			if raw == "" {
				if f.required {
					return nil, fmt.Errorf("%s requires a With JSON object", action)
				}
				continue
			}
			if err := json.Unmarshal([]byte(raw), &base); err != nil {
				return nil, fmt.Errorf("With is not valid JSON: %w", err)
			}
			continue
		}

		if raw == "" {
			if f.required {
				return nil, fmt.Errorf("%s is required", f.label)
			}
			continue
		}

		switch f.kind {
		case stepFieldInt:
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%s must be a non-negative integer", f.label)
			}
			with[f.key] = n
		case stepFieldEnv:
			env, err := parseEnvTokens(raw, f.label)
			if err != nil {
				return nil, err
			}
			with[f.key] = env
		case stepFieldVolumeAttach:
			with[f.key] = volumeAttachWith(raw)
		case stepFieldList:
			with[f.key] = splitList(raw)
		default: // text, image
			with[f.key] = raw
		}
	}

	// Overlay extra keys from the advanced With JSON without clobbering typed fields.
	for k, v := range base {
		if _, set := with[k]; !set {
			with[k] = v
		}
	}

	// A create-volume step is always scoped to the run: default its workflow_id to
	// ${run_id} so the user never has to type it (an explicit value still wins).
	if action == "forge/create-volume" {
		if _, set := with["workflow_id"]; !set {
			with["workflow_id"] = volumeRunIDRef
		}
	}
	if action == "forge/build-image" {
		nestForgeBuild(with)
	}
	if action == "forge/git-clone" {
		nestForgeCheckout(with)
	}
	return with, nil
}

// checkoutFields are the flat git-clone form keys that nest under the `checkout`
// object of a forge/git-clone /executions body (image and volumes stay top-level).
var checkoutFields = []string{"path", "ref", "depth"}

// nestForgeCheckout restructures the flat git-clone form fields into forge's request:
// the path/ref/depth keys under a `checkout` object (always present so forge runs the
// clone prologue), plus a no-op `run` default so the execution is a valid shell command
// the checkout weaves into. The repo is wired per-occurrence as secret_refs.GIT_CLONE_URL.
func nestForgeCheckout(with map[string]any) {
	checkout := map[string]any{}
	for _, k := range checkoutFields {
		if v, ok := with[k]; ok {
			checkout[k] = v
			delete(with, k)
		}
	}
	with["checkout"] = checkout
	if _, ok := with["run"]; !ok {
		with["run"] = "true"
	}
}

// buildImageFields are the flat form keys that nest under the `build` object of a
// forge/build-image /executions body (volumes and runner_class stay top-level).
var buildImageFields = []string{"destinations", "dockerfile", "context", "build_args", "target"}

// flattenStepWith is the inverse of the action-specific nesting done by buildStepWith:
// it returns a shallow copy of `with` with build-image's `build` object and registry
// secret_ref lifted back to the flat form fields, so the edit form pre-populates and
// round-trips. Other actions are returned as a plain copy.
func flattenStepWith(action string, with map[string]any) map[string]any {
	wm := make(map[string]any, len(with))
	for k, v := range with {
		wm[k] = v
	}
	if action == "forge/git-clone" {
		if checkout, ok := wm["checkout"].(map[string]any); ok {
			for k, v := range checkout {
				wm[k] = v
			}
			delete(wm, "checkout")
		}
		return wm
	}
	if action != "forge/build-image" {
		return wm
	}
	if build, ok := wm["build"].(map[string]any); ok {
		for k, v := range build {
			wm[k] = v
		}
		delete(wm, "build")
	}
	if sr, ok := wm["secret_refs"].(map[string]any); ok {
		if ref, ok := sr[registryAuthEnv].(string); ok {
			wm["registry_secret"] = strings.TrimPrefix(ref, "secret:")
			rest := map[string]any{}
			for k, v := range sr {
				if k != registryAuthEnv {
					rest[k] = v
				}
			}
			if len(rest) > 0 {
				wm["secret_refs"] = rest
			} else {
				delete(wm, "secret_refs")
			}
		}
	}
	return wm
}

// formatList renders a stored list value ([]any of strings, from JSON, or []string)
// back to the comma-separated text the list field reads.
func formatList(v any) string {
	switch t := v.(type) {
	case []string:
		return strings.Join(t, ", ")
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	default:
		return ""
	}
}

// nestForgeBuild restructures the flat build-image form fields into forge's nested
// request: the build.* keys under a `build` object, and the chosen registry secret
// into secret_refs[REGISTRY_AUTH].
func nestForgeBuild(with map[string]any) {
	build := map[string]any{}
	for _, k := range buildImageFields {
		if v, ok := with[k]; ok {
			build[k] = v
			delete(with, k)
		}
	}
	if len(build) > 0 {
		with["build"] = build
	}
	if rs, ok := with["registry_secret"].(string); ok {
		delete(with, "registry_secret")
		if rs != "" {
			sr, _ := with["secret_refs"].(map[string]any)
			if sr == nil {
				sr = map[string]any{}
			}
			sr[registryAuthEnv] = "secret:" + rs
			with["secret_refs"] = sr
		}
	}
}

// volumeAttachWith turns a compact "name[:/mount]" attach spec into forge's volumes
// array: one shared volume, scoped to the run (${run_id}), made the working dir.
func volumeAttachWith(raw string) []any {
	name, mount, _ := strings.Cut(raw, ":")
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultVolumeName
	}
	mount = strings.TrimSpace(mount)
	if mount == "" {
		mount = defaultMountPath
	}
	return []any{map[string]any{
		"workflow_id": volumeRunIDRef,
		"name":        name,
		"mount_path":  mount,
		"workdir":     true,
	}}
}

// volumeAttachString reverses volumeAttachWith for the edit form: it renders the
// first attached volume back to "name" or "name:/mount". Returns "" when the value
// isn't a recognisable single-volume attach (leaving it to the advanced With field).
func volumeAttachString(v any) string {
	arr, ok := v.([]any)
	if !ok || len(arr) != 1 {
		return ""
	}
	m, ok := arr[0].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := m["name"].(string)
	if name == "" {
		return ""
	}
	mount, _ := m["mount_path"].(string)
	if mount == "" || mount == defaultMountPath {
		return name
	}
	return name + ":" + mount
}

// parseEnvTokens splits a KEY=VALUE token string into a map, sharing the shell-style
// tokeniser used by the forge command form.
func parseEnvTokens(s, label string) (map[string]string, error) {
	toks, err := parseCommandLine(s)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, kv := range toks {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("invalid %s %q: expected KEY=VALUE", label, kv)
		}
		env[k] = v
	}
	return env, nil
}
