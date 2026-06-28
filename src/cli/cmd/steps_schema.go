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
	stepFieldText stepFieldKind = iota // plain string
	stepFieldInt                       // non-negative integer
	stepFieldEnv                       // KEY=VALUE tokens → map[string]string
	stepFieldJSON                      // raw JSON object, merged as the with base
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
	return with, nil
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
