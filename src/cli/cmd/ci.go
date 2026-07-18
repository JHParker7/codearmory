package cmd

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// ── Shared types ──────────────────────────────────────────────────────────────

// stepDef mirrors the Step response from the API.
type stepDef struct {
	StepID string `json:"step_id"`
	Name   string `json:"name"`
}

// matrixConfig fans a step out into one execution per value in a list, binding
// ${matrix.<var>} per execution.
type matrixConfig struct {
	Var        string   `json:"var"`
	Values     []string `json:"values,omitempty"`
	ValuesFrom string   `json:"values_from,omitempty"`
	// MaxConcurrent caps how many fan-out executions run at once (0 = the service
	// default). Lower it when each value spins up a resource-heavy runner.
	MaxConcurrent int `json:"max_concurrent,omitempty"`
	// Sequential runs the values one at a time instead of in parallel (the simple
	// form of max_concurrent: 1); takes precedence over MaxConcurrent.
	Sequential bool `json:"sequential,omitempty"`
}

// scatterConfig fans a step out over a set of workspace paths — each runs the step on
// its OWN clone of the workspace, and after all legs finish their declared owned
// outputs are gathered back into the base as a disjoint union. The path set comes from
// exactly one of regex (a workspace scan) or paths_from (a ${...} list from an input or
// earlier step). Mirrors the workflows ScatterConfig; mutually exclusive with matrix,
// and requires an inline step action.
type scatterConfig struct {
	Volume        string   `json:"volume,omitempty"`     // base workspace volume; default "workspace"
	MountPath     string   `json:"mount_path,omitempty"` // where the workspace mounts; default "/workspace"
	Regex         string   `json:"regex,omitempty"`      // regex source: POSIX ERE scan of the workspace
	Mode          string   `json:"mode,omitempty"`       // "dir" (default) | "file"
	MaxDepth      int      `json:"max_depth,omitempty"`  // find -maxdepth; 0/unset = unlimited
	PathsFrom     string   `json:"paths_from,omitempty"` // list source: ${...} ref (input/step output), mutually exclusive with regex
	Outputs       []string `json:"outputs,omitempty"`    // owned paths gathered back; may ref ${scatter.path}
	SizeMB        int64    `json:"size_mb,omitempty"`    // per-leg clone volume size
	Medium        string   `json:"medium,omitempty"`     // "memory" (default) | "disk"
	MaxConcurrent int      `json:"max_concurrent,omitempty"`
}

// approvalGate is an inline manual-approval pause on a pipeline step ref — no
// separate step is needed. A ref carries either a step_id or an approval gate.
type approvalGate struct {
	Message   string   `json:"message,omitempty"`
	Approvers []string `json:"approvers,omitempty"`
}

// workflowStepRef is the per-step payload inside a create/update workflow request.
// It is exactly one of three kinds: a stored-step reference (StepID), an INLINE step
// whose definition lives on the ref itself (Action, no StepID — private to this
// pipeline), or an inline approval gate (Approval).
//
// For a reference, With holds per-occurrence overrides merged over the step definition
// at run time (the backend lets ref keys win) — how a pipeline assigns a repo to a
// reusable step without baking it into the shared definition (see gitRepoStepWith /
// the DSL's `name@repo` syntax). For an inline step, Name is the step name, With is
// its full config, and Timeout is the per-step timeout.
type workflowStepRef struct {
	StepID  string         `json:"step_id,omitempty"`
	Action  string         `json:"action,omitempty"`
	Timeout int64          `json:"timeout,omitempty"`
	Name    string         `json:"name,omitempty"`
	With    map[string]any `json:"with,omitempty"`
	Matrix  *matrixConfig  `json:"matrix,omitempty"`
	Scatter *scatterConfig `json:"scatter,omitempty"`
	// MapID puts this step inside the named map region: the region's body runs once
	// per value. The region's own definition (var, values, volume) comes from --map.
	MapID    string        `json:"map_id,omitempty"`
	Approval *approvalGate `json:"approval,omitempty"`
}

// workflowRoute is a directed edge between two steps, by name. Routes are how a
// pipeline expresses a fork or a join — two edges out of one step run it two ways.
// A pipeline with no routes is a plain sequence.
type workflowRoute struct {
	From string `json:"from"`
	To   string `json:"to"`
	When string `json:"when,omitempty"`
}

// gitCloneEnv is the env var a per-step git repo is injected as. forge resolves the
// `git:<url>` secret_ref under this name into an authenticated clone URL at dispatch,
// so the runner clones/pulls/pushes via $GIT_CLONE_URL. Mirrors the portal builder.
const gitCloneEnv = "GIT_CLONE_URL"

// gitRepoStepWith wraps a per-step repo (a clone URL or a ${inputs.X} template) as
// the step-ref `with` override forge consumes — secret_refs.GIT_CLONE_URL = git:<v>.
// Returns nil for an empty repo so the ref carries no override.
func gitRepoStepWith(repo string) map[string]any {
	if repo == "" {
		return nil
	}
	return map[string]any{"secret_refs": map[string]any{gitCloneEnv: "git:" + repo}}
}

// gitRepoFromStepWith reverses gitRepoStepWith: it reads the bare repo (a clone URL
// or ${inputs.X}) back out of a step's `with.secret_refs.GIT_CLONE_URL`, stripping the
// git: scheme. Returns "" when there is no git: ref, OR when secret_refs carries keys
// beyond GIT_CLONE_URL (the DSL's `@repo` can't express those, so it leaves them be
// rather than silently dropping them on round-trip — use -f JSON for richer refs).
func gitRepoFromStepWith(with map[string]any) string {
	sr, ok := with["secret_refs"].(map[string]any)
	if !ok || len(sr) != 1 {
		return ""
	}
	ref, ok := sr[gitCloneEnv].(string)
	if !ok {
		return ""
	}
	bare, ok := strings.CutPrefix(ref, "git:")
	if !ok {
		return ""
	}
	return bare
}

// pipelineInputDef declares a pipeline input parameter. The workflows backend
// applies the default when an input is omitted at trigger time and rejects a run
// that omits a required input with no default.
type pipelineInputDef struct {
	Name        string `json:"name"`
	Default     string `json:"default,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// pipelineOutputDef declares a pipeline output: a name and a ${...} template value
// (e.g. ${steps.STEP.output.KEY}). When this pipeline is invoked via the
// workflows/trigger action, its outputs form the triggering step's output.
type pipelineOutputDef struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// pipelineFile is the JSON file format for -f pipeline creation/update. inputs and
// outputs are optional pipeline-level input/output declarations.
type pipelineFile struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Steps       []workflowStepRef   `json:"steps,omitempty"`
	Inputs      []pipelineInputDef  `json:"inputs,omitempty"`
	Outputs     []pipelineOutputDef `json:"outputs,omitempty"`
	// Routes are the edges between steps — omit for a plain sequence. Maps declare
	// the map regions steps join via map_id. Ticket mirrors every run of the pipeline
	// into a ticket (there are no ticket steps — it is a property of the pipeline).
	Routes []workflowRoute  `json:"routes,omitempty"`
	Maps   []map[string]any `json:"maps,omitempty"`
	Ticket map[string]any   `json:"ticket,omitempty"`
}

// ── DSL parser ────────────────────────────────────────────────────────────────

// dslStage is one rank inside a segment: names that run concurrently.
type dslStage struct {
	names []string
	repos []string // parallel to names; repos[i] is the git repo for names[i] ("" = none)
}

// dslNode is one "->"-separated segment: a body of one or more stages, optionally
// marked as a map region.
type dslNode struct {
	stages []dslStage
	// mapID, when set, puts every step of this segment in that map region — the body
	// runs once per value. The region's definition comes from --map.
	mapID string
}

func (n dslNode) entries() []string { return n.stages[0].names }
func (n dslNode) exits() []string   { return n.stages[len(n.stages)-1].names }

// mapIDRe restricts a map region name to the charset the API accepts.
var mapIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// splitTopLevel splits on "->" only OUTSIDE brackets, so a map body can itself use
// "->" ("[build->test]*per-module"). A naive strings.Split would tear that body apart.
func splitTopLevel(dsl string) ([]string, error) {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(dsl); i++ {
		switch dsl[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unexpected ']' in pipeline DSL: %q", dsl)
			}
		case '-':
			if depth == 0 && i+1 < len(dsl) && dsl[i+1] == '>' {
				out = append(out, dsl[start:i])
				i++
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unclosed '[' in pipeline DSL: %q", dsl)
	}
	return append(out, dsl[start:]), nil
}

// splitStepRepo splits a DSL step token "name@repo" into its bare step name and the
// optional per-step git repo (a clone URL or a ${inputs.X} template). Step names are
// [A-Za-z0-9._-] so they never contain '@'; the split is on the first '@', leaving any
// '@' inside a repo URL intact. Returns ("", "") guarded by the caller's empty check.
func splitStepRepo(tok string) (name, repo string) {
	name, repo, _ = strings.Cut(tok, "@")
	return strings.TrimSpace(name), strings.TrimSpace(repo)
}

// parseStage parses one comma-separated rank of "name@repo" tokens.
func parseStage(seg, ctx string) (dslStage, error) {
	var st dslStage
	for _, p := range strings.Split(seg, ",") {
		name, repo := splitStepRepo(p)
		if name == "" {
			return st, fmt.Errorf("empty step name in %s", ctx)
		}
		st.names = append(st.names, name)
		st.repos = append(st.repos, repo)
	}
	return st, nil
}

// parseDSL tokenises a pipeline DSL string into ordered nodes.
//
// Grammar: steps are separated by "->". A bracket holds a body, whose stages are
// themselves separated by "->" and whose concurrent steps are separated by ",".
// A bracket suffixed with "*<map-id>" is a MAP REGION: its body runs once per value
// of that region (defined with --map). A step may carry a per-step git repo as
// "name@<clone-url-or-${inputs.X}>", cloned as $GIT_CLONE_URL for that step only.
//
// Parallelism is not a field on a step — it is the shape of the graph, so a bracket
// simply compiles to routes that fork and re-join.
//
// Examples:
//
//	"build->test->deploy"                      — three sequential steps
//	"build->[lint,test]->deploy"               — lint and test run in parallel
//	"checkout->[build->test]*per-module"       — build then test, once per module
//	"checkout->[lint,test]*per-module"         — lint and test in parallel, per module
//	"test@https://github.com/acme/app.git"     — test clones that repo
//	"test@${inputs.REPO}"                      — test clones a run-input repo
func parseDSL(dsl string) ([]dslNode, error) {
	segments, err := splitTopLevel(dsl)
	if err != nil {
		return nil, err
	}
	var nodes []dslNode
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return nil, fmt.Errorf("empty segment in pipeline DSL")
		}
		if !strings.HasPrefix(seg, "[") {
			st, err := parseStage(seg, "pipeline DSL")
			if err != nil {
				return nil, err
			}
			if len(st.names) > 1 {
				return nil, fmt.Errorf("parallel steps must be bracketed: %q — write [%s]", seg, seg)
			}
			nodes = append(nodes, dslNode{stages: []dslStage{st}})
			continue
		}
		close := strings.LastIndex(seg, "]")
		if close < 0 {
			return nil, fmt.Errorf("unclosed '[' in pipeline DSL: %q", seg)
		}
		inner := seg[1:close]
		if strings.Contains(inner, "[") {
			return nil, fmt.Errorf("nested '[' is not supported: %q", seg)
		}
		node := dslNode{}
		// A "*<map-id>" suffix turns the bracket into a map region.
		if suffix := strings.TrimSpace(seg[close+1:]); suffix != "" {
			id, ok := strings.CutPrefix(suffix, "*")
			if !ok {
				return nil, fmt.Errorf("unexpected %q after ']' in %q — did you mean *<map-id>?", suffix, seg)
			}
			if !mapIDRe.MatchString(id) {
				return nil, fmt.Errorf("invalid map id %q in %q", id, seg)
			}
			node.mapID = id
		}
		for _, stageSeg := range strings.Split(inner, "->") {
			stageSeg = strings.TrimSpace(stageSeg)
			if stageSeg == "" {
				return nil, fmt.Errorf("empty stage in %q", seg)
			}
			st, err := parseStage(stageSeg, fmt.Sprintf("%q", seg))
			if err != nil {
				return nil, err
			}
			node.stages = append(node.stages, st)
		}
		// A plain bracket exists only to fork; one step in it is just that step. A map
		// bracket is meaningful with a single-step body (run it once per value).
		if node.mapID == "" {
			var total int
			for _, st := range node.stages {
				total += len(st.names)
			}
			if total < 2 {
				return nil, fmt.Errorf("parallel group must contain at least two steps: %q", seg)
			}
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("pipeline DSL produced no steps")
	}
	return nodes, nil
}

// resolveStepName looks up a step by name and returns its ID.
func resolveStepName(name string) (string, error) {
	data, err := doRequest("GET", "/workflows/steps?name="+url.QueryEscape(name), nil)
	if err != nil {
		return "", fmt.Errorf("looking up step %q: %w", name, err)
	}
	var steps []stepDef
	if err := json.Unmarshal(data, &steps); err != nil {
		return "", fmt.Errorf("parsing step response for %q: %w", name, err)
	}
	if len(steps) == 0 {
		return "", fmt.Errorf("step %q not found — create it first:\n  armory pipelines create step %s --service <svc> --path <path>", name, name)
	}
	return steps[0].StepID, nil
}

// dslToRefs resolves DSL nodes to workflow step refs and the routes between them.
//
// The refs are the node set; the routes are the edges. Within a segment consecutive
// stages are joined by a cross product (every step of stage k+1 waits for every step
// of stage k), and between segments the previous segment's exits fan into the next
// segment's entries — which is exactly what "->" means.
func dslToRefs(nodes []dslNode) ([]workflowStepRef, []workflowRoute, error) {
	var refs []workflowStepRef
	for _, node := range nodes {
		for _, st := range node.stages {
			for i, name := range st.names {
				id, err := resolveStepName(name)
				if err != nil {
					return nil, nil, err
				}
				// Name is set explicitly: it is what routes address, and what
				// ${steps.<name>.output} resolves against.
				ref := workflowStepRef{StepID: id, Name: name, MapID: node.mapID}
				if i < len(st.repos) {
					ref.With = gitRepoStepWith(st.repos[i])
				}
				refs = append(refs, ref)
			}
		}
	}

	var routes []workflowRoute
	link := func(from, to []string) {
		for _, p := range from {
			for _, c := range to {
				routes = append(routes, workflowRoute{From: p, To: c})
			}
		}
	}
	for _, node := range nodes {
		for k := 1; k < len(node.stages); k++ {
			link(node.stages[k-1].names, node.stages[k].names)
		}
	}
	for k := 1; k < len(nodes); k++ {
		link(nodes[k-1].exits(), nodes[k].entries())
	}
	return refs, routes, nil
}

// pipelineMapFlags collects --map values: each is a MapDef as JSON. The DSL names a
// region ("[build->test]*per-module"); this is where that region is actually defined
// (its var, its values, its per-iteration volume).
var pipelineMapFlags []string

// parseMapFlags turns each --map value into a map region definition.
func parseMapFlags(flags []string) ([]map[string]any, error) {
	var out []map[string]any
	for _, f := range flags {
		var m map[string]any
		if err := json.Unmarshal([]byte(f), &m); err != nil {
			return nil, fmt.Errorf("parsing --map %q: %w (expected JSON, e.g. %s)", f, err,
				`{"id":"per-module","var":"module","values_from":"${steps.discover.output.MODULES}"}`)
		}
		if id, _ := m["id"].(string); id == "" {
			return nil, fmt.Errorf("--map %q: an \"id\" is required — it is what the DSL's [body]*<id> refers to", f)
		}
		out = append(out, m)
	}
	return out, nil
}

// ── Raw pipeline helpers (convert-step / localize-step) ─────────────────────────

// rawPipeline is a pipeline fetched with ?raw=true, exposing the stored, unenriched
// step refs (the normal GET returns enriched steps with merged `with`, which would
// bake a referenced step's definition into its override on re-PUT).
type rawPipeline struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Inputs      []pipelineInputDef  `json:"inputs,omitempty"`
	Outputs     []pipelineOutputDef `json:"outputs,omitempty"`
	StepRefs    []workflowStepRef   `json:"step_refs"`
	// Routes, Maps and Ticket must round-trip: none of them live in the step array, so
	// a re-PUT that dropped them would change what the pipeline DOES while only meaning
	// to edit a step — flattening a graph into a sequence, or silently turning off the
	// ticket mirroring every run of it depends on.
	Routes []workflowRoute  `json:"routes,omitempty"`
	Maps   []map[string]any `json:"maps,omitempty"`
	Ticket map[string]any   `json:"ticket,omitempty"`
}

func fetchRawPipeline(id string) (rawPipeline, error) {
	data, err := doRequest("GET", "/workflows/pipelines/"+id+"?raw=true", nil)
	if err != nil {
		return rawPipeline{}, fmt.Errorf("fetching pipeline: %w", err)
	}
	var pl rawPipeline
	if err := json.Unmarshal(data, &pl); err != nil {
		return rawPipeline{}, fmt.Errorf("parsing pipeline: %w", err)
	}
	return pl, nil
}

func putRawPipeline(id string, pl rawPipeline) error {
	payload := map[string]any{"name": pl.Name, "steps": pl.StepRefs}
	if pl.Description != "" {
		payload["description"] = pl.Description
	}
	if len(pl.Inputs) > 0 {
		payload["inputs"] = pl.Inputs
	}
	if len(pl.Outputs) > 0 {
		payload["outputs"] = pl.Outputs
	}
	if len(pl.Routes) > 0 {
		payload["routes"] = pl.Routes
	}
	if len(pl.Maps) > 0 {
		payload["maps"] = pl.Maps
	}
	if len(pl.Ticket) > 0 {
		payload["ticket"] = pl.Ticket
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return apiCall("PUT", "/workflows/pipelines/"+id, body)
}

// fullStepDef is a stored step's definition, as returned by GET /workflows/steps/{id}.
type fullStepDef struct {
	StepID  string         `json:"step_id"`
	Name    string         `json:"name"`
	Action  string         `json:"action"`
	With    map[string]any `json:"with"`
	Timeout int64          `json:"timeout"`
}

func getStepDef(id string) (fullStepDef, error) {
	data, err := doRequest("GET", "/workflows/steps/"+id, nil)
	if err != nil {
		return fullStepDef{}, err
	}
	var s fullStepDef
	if err := json.Unmarshal(data, &s); err != nil {
		return fullStepDef{}, err
	}
	return s, nil
}

// ── Command tree ──────────────────────────────────────────────────────────────

var ciCmd = &cobra.Command{
	Use:   "pipelines",
	Short: "Manage pipelines, steps and runs",
}

var ciCreateCmd = &cobra.Command{Use: "create", Short: "Create a pipeline resource"}
var ciListCmd = &cobra.Command{Use: "list", Short: "List pipeline resources"}
var ciGetCmd = &cobra.Command{Use: "get", Short: "Get a pipeline resource"}
var ciUpdateCmd = &cobra.Command{Use: "update", Short: "Update a pipeline resource"}
var ciDeleteCmd = &cobra.Command{Use: "delete", Short: "Delete a pipeline resource"}
var ciRunCmd = &cobra.Command{Use: "run", Short: "Trigger a pipeline run"}
var ciCancelCmd = &cobra.Command{Use: "cancel", Short: "Cancel a pipeline run"}
var ciApproveCmd = &cobra.Command{Use: "approve", Short: "Approve a pipeline run paused on a manual gate"}
var ciRejectCmd = &cobra.Command{Use: "reject", Short: "Reject a pipeline run paused on a manual gate"}

func init() {
	// ── armory pipelines create step ─────────────────────────────────────────────────

	var (
		stepFile        string
		stepAction      string
		stepWith        string
		stepImage       string
		stepRun         string
		stepRunnerClass string
		stepEnvs        []string
		stepDescription string
		stepTimeout     int64
	)

	createStepCmd := &cobra.Command{
		Use:   "step <name>",
		Short: "Create a reusable pipeline step",
		Long: `Create a reusable step definition that can be composed into pipelines.

Use "armory pipelines list actions" to see all registered catalog actions.

  # Run a shell command inside a container via forge (convenience flags):
  armory pipelines create step unit_tests \
    --action forge/run \
    --image ubuntu:22.04 \
    --run "go test ./..." \
    --timeout 300

  # Catalog action with explicit --with JSON:
  armory pipelines create step open_ticket \
    --action tickets/create \
    --with '{"title":"Build failed: ${HOOK_REPO}","priority":"high"}'

  # Raw HTTP call (escape hatch — calls any registered service):
  armory pipelines create step notify \
    --action http \
    --with '{"service":"conductor","path":"/webhook","method":"POST"}'

  # From a JSON file (-f):
  armory pipelines create step my_step -f step.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			var payload map[string]any

			if stepFile != "" {
				data, err := os.ReadFile(stepFile)
				if err != nil {
					return fmt.Errorf("reading step file: %w", err)
				}
				if err := json.Unmarshal(data, &payload); err != nil {
					return fmt.Errorf("parsing step file: %w", err)
				}
				if _, ok := payload["name"]; !ok {
					payload["name"] = name
				}
			} else {
				if stepAction == "" {
					return fmt.Errorf("--action is required (run \"armory pipelines list actions\" to see available actions)")
				}
				with, err := buildWith(stepAction, stepWith, stepImage, stepRun, stepRunnerClass, stepEnvs)
				if err != nil {
					return err
				}
				payload = map[string]any{
					"name":        name,
					"description": stepDescription,
					"action":      stepAction,
					"with":        with,
					"timeout":     stepTimeout,
				}
			}

			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/workflows/steps", body)
		},
	}
	createStepCmd.Flags().StringVarP(&stepFile, "file", "f", "", "JSON step definition file")
	createStepCmd.Flags().StringVar(&stepAction, "action", "", "Action name (run \"armory pipelines list actions\" to see available actions)")
	createStepCmd.Flags().StringVar(&stepWith, "with", "", "Step configuration as a JSON object")
	createStepCmd.Flags().StringVar(&stepImage, "image", "", "Container image (forge/run convenience flag, e.g. ubuntu:22.04)")
	createStepCmd.Flags().StringVar(&stepRun, "run", "", "Shell command to run (forge/run convenience flag)")
	createStepCmd.Flags().StringVar(&stepRunnerClass, "runner-class", "", "Forge runner class (forge/run convenience flag, e.g. large; defaults to standard)")
	createStepCmd.Flags().StringArrayVar(&stepEnvs, "env", nil, "Env var KEY=VALUE (forge/run convenience flag, repeatable)")
	createStepCmd.Flags().StringVar(&stepDescription, "description", "", "Human-readable description")
	createStepCmd.Flags().Int64Var(&stepTimeout, "timeout", 30, "Step timeout in seconds")
	ciCreateCmd.AddCommand(createStepCmd)

	// ── armory pipelines list steps ──────────────────────────────────────────────────

	ciListCmd.AddCommand(&cobra.Command{
		Use:   "steps",
		Short: "List reusable steps",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/workflows/steps", nil) },
	})

	// ── armory pipelines get step ────────────────────────────────────────────────────

	ciGetCmd.AddCommand(&cobra.Command{
		Use:   "step <id>",
		Short: "Get a step",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/workflows/steps/"+args[0], nil)
		},
	})

	// ── armory pipelines update step ─────────────────────────────────────────────────

	var (
		updateStepFile        string
		updateStepAction      string
		updateStepWith        string
		updateStepImage       string
		updateStepRun         string
		updateStepRunnerClass string
		updateStepEnvs        []string
		updateStepDescription string
		updateStepTimeout     int64
	)

	updateStepCmd := &cobra.Command{
		Use:   "step <id>",
		Short: "Update a step (affects all pipelines that reference it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var payload map[string]any

			if updateStepFile != "" {
				data, err := os.ReadFile(updateStepFile)
				if err != nil {
					return fmt.Errorf("reading step file: %w", err)
				}
				if err := json.Unmarshal(data, &payload); err != nil {
					return fmt.Errorf("parsing step file: %w", err)
				}
			} else {
				if updateStepAction == "" {
					return fmt.Errorf("--action is required (or use -f <file>)")
				}
				with, err := buildWith(updateStepAction, updateStepWith, updateStepImage, updateStepRun, updateStepRunnerClass, updateStepEnvs)
				if err != nil {
					return err
				}
				payload = map[string]any{
					"description": updateStepDescription,
					"action":      updateStepAction,
					"with":        with,
					"timeout":     updateStepTimeout,
				}
			}

			// PUT requires name; fetch current step name if not in payload.
			if _, ok := payload["name"]; !ok {
				data, err := doRequest("GET", "/workflows/steps/"+args[0], nil)
				if err != nil {
					return fmt.Errorf("fetching current step: %w", err)
				}
				var current struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(data, &current); err != nil || current.Name == "" {
					return fmt.Errorf("could not determine step name; pass it in -f")
				}
				payload["name"] = current.Name
			}

			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/workflows/steps/"+args[0], body)
		},
	}
	updateStepCmd.Flags().StringVarP(&updateStepFile, "file", "f", "", "JSON step definition file")
	updateStepCmd.Flags().StringVar(&updateStepAction, "action", "", "Action name (run \"armory pipelines list actions\" to see available actions)")
	updateStepCmd.Flags().StringVar(&updateStepWith, "with", "", "Step configuration as a JSON object")
	updateStepCmd.Flags().StringVar(&updateStepImage, "image", "", "Container image (forge/run convenience flag)")
	updateStepCmd.Flags().StringVar(&updateStepRun, "run", "", "Shell command to run (forge/run convenience flag)")
	updateStepCmd.Flags().StringVar(&updateStepRunnerClass, "runner-class", "", "Forge runner class (forge/run convenience flag, e.g. large; defaults to standard)")
	updateStepCmd.Flags().StringArrayVar(&updateStepEnvs, "env", nil, "Env var KEY=VALUE (forge/run convenience flag, repeatable)")
	updateStepCmd.Flags().StringVar(&updateStepDescription, "description", "", "Human-readable description")
	updateStepCmd.Flags().Int64Var(&updateStepTimeout, "timeout", 30, "Step timeout in seconds")
	ciUpdateCmd.AddCommand(updateStepCmd)

	// ── armory pipelines delete step ─────────────────────────────────────────────────

	ciDeleteCmd.AddCommand(&cobra.Command{
		Use:   "step <id>",
		Short: "Delete a step",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/workflows/steps/"+args[0], nil)
		},
	})

	// ── armory pipelines create pipeline ─────────────────────────────────────────────

	var pipelineFileFlag string

	createPipelineCmd := &cobra.Command{
		Use:   "pipeline <repo> <branch> [dsl]",
		Short: "Create a CI/CD pipeline",
		Long: `Create a pipeline by composing existing steps.

DSL syntax — step names must match previously-created steps:
  armory pipelines create pipeline myrepo main push->unit_tests->deploy

  # [a,b] runs a and b concurrently, then re-joins:
  armory pipelines create pipeline myrepo main "push->[unit_tests,security_scan]->deploy"

  # [body]*<map-id> runs the body once per value of a map region:
  armory pipelines create pipeline myrepo main "checkout->[build->test]*per-module" \
    --map '{"id":"per-module","var":"module","values_from":"${steps.discover.output.MODULES}"}'

Parallelism is the shape of the graph, not a field on a step: a bracket compiles to
routes that fork and re-join. Author routes directly with -f for anything the one-line
DSL cannot say (a join across non-adjacent steps, or a conditional edge).

JSON file (-f) — a step is a stored-step reference, an inline step, or a gate;
"routes" are the edges between them (omit for a plain sequence):
  {
    "name": "optional name",
    "steps": [
      {"step_id": "<id>", "name": "push"},
      {"action": "forge/run", "name": "build", "with": {"image": "alpine", "run": "make"}},
      {"action": "forge/run", "name": "test", "with": {"run": "make test"}},
      {"approval": {"message": "deploy to prod?", "name": "gate"}}
    ],
    "routes": [
      {"from": "push", "to": "build"},
      {"from": "push", "to": "test"},
      {"from": "build", "to": "gate"},
      {"from": "test", "to": "gate"},
      {"from": "gate", "to": "deploy", "when": "steps.build.status == 'completed'"}
    ]
  }

An inline step ({action,name,with,...}) is private to this pipeline — no separate
step is created. Use "armory pipelines convert-step <id> <name>" to promote it to a
reusable step, or "localize-step" to copy a shared step's definition inline.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("requires <repo> and <branch>")
			}
			if pipelineFileFlag == "" && len(args) < 3 {
				return fmt.Errorf("requires a DSL string or -f <file>")
			}
			if pipelineFileFlag != "" && len(args) > 2 {
				return fmt.Errorf("cannot use both DSL string and -f")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, branch := args[0], args[1]

			var payload struct {
				Name        string              `json:"name"`
				Description string              `json:"description"`
				Project     string              `json:"project,omitempty"`
				Steps       []workflowStepRef   `json:"steps,omitempty"`
				Inputs      []pipelineInputDef  `json:"inputs,omitempty"`
				Outputs     []pipelineOutputDef `json:"outputs,omitempty"`
				Routes      []workflowRoute     `json:"routes,omitempty"`
				Maps        []map[string]any    `json:"maps,omitempty"`
				Ticket      map[string]any      `json:"ticket,omitempty"`
			}

			if pipelineFileFlag != "" {
				data, err := os.ReadFile(pipelineFileFlag)
				if err != nil {
					return fmt.Errorf("reading pipeline file: %w", err)
				}
				var pf pipelineFile
				if err := json.Unmarshal(data, &pf); err != nil {
					return fmt.Errorf("parsing pipeline file: %w", err)
				}
				payload.Name = pf.Name
				payload.Description = pf.Description
				payload.Steps = pf.Steps
				payload.Inputs = pf.Inputs
				payload.Outputs = pf.Outputs
				payload.Routes = pf.Routes
				payload.Maps = pf.Maps
				payload.Ticket = pf.Ticket
			} else {
				nodes, err := parseDSL(args[2])
				if err != nil {
					return err
				}
				refs, rts, err := dslToRefs(nodes)
				if err != nil {
					return err
				}
				payload.Steps = refs
				payload.Routes = rts
				mp, err := parseMapFlags(pipelineMapFlags)
				if err != nil {
					return err
				}
				payload.Maps = mp
			}

			if payload.Name == "" {
				payload.Name = repo + "/" + branch
			}
			if payload.Description == "" {
				payload.Description = "Pipeline for " + repo + " on " + branch
			}
			// Tag the new pipeline with the project the user is working in, unless
			// overridden by --project or suppressed by --all.
			payload.Project = projectFilter()

			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/workflows/pipelines", body)
		},
	}
	createPipelineCmd.Flags().StringVarP(&pipelineFileFlag, "file", "f", "", "JSON pipeline definition file")
	createPipelineCmd.Flags().StringArrayVar(&pipelineMapFlags, "map", nil, `define a map region the DSL refers to as [body]*<id>, as JSON (repeatable): {"id":"per-module","var":"module","values_from":"${steps.discover.output.MODULES}"}`)
	ciCreateCmd.AddCommand(createPipelineCmd)

	// ── armory pipelines list pipelines ──────────────────────────────────────────────

	ciListCmd.AddCommand(&cobra.Command{
		Use:   "pipelines",
		Short: "List pipelines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", appendProjectParam("/workflows/pipelines"), nil)
		},
	})

	// ── armory pipelines get pipeline ────────────────────────────────────────────────

	ciGetCmd.AddCommand(&cobra.Command{
		Use:   "pipeline <id>",
		Short: "Get a pipeline with its full step definitions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/workflows/pipelines/"+args[0], nil)
		},
	})

	// ── armory pipelines delete pipeline ─────────────────────────────────────────────

	ciDeleteCmd.AddCommand(&cobra.Command{
		Use:   "pipeline <id>",
		Short: "Delete a pipeline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/workflows/pipelines/"+args[0], nil)
		},
	})

	// ── armory pipelines run pipeline ────────────────────────────────────────────────

	var runInputs []string
	runPipelineCmd := &cobra.Command{
		Use:   "pipeline <id>",
		Short: "Trigger a pipeline run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inputs := map[string]string{}
			for _, kv := range runInputs {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("invalid input %q: expected key=value", kv)
				}
				inputs[k] = v
			}
			body, err := json.Marshal(map[string]any{"inputs": inputs})
			if err != nil {
				return err
			}
			return apiCall("POST", "/workflows/pipelines/"+args[0]+"/runs", body)
		},
	}
	runPipelineCmd.Flags().StringArrayVarP(&runInputs, "input", "i", nil, "Input variable (key=value, repeatable)")
	ciRunCmd.AddCommand(runPipelineCmd)

	// ── armory pipelines list runs ───────────────────────────────────────────────────

	var listRunsPipeline string
	listRunsCmd := &cobra.Command{
		Use:   "runs",
		Short: "List pipeline runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/workflows/runs"
			if listRunsPipeline != "" {
				path += "?workflow_id=" + url.QueryEscape(listRunsPipeline)
			}
			return apiCall("GET", path, nil)
		},
	}
	listRunsCmd.Flags().StringVar(&listRunsPipeline, "pipeline", "", "Filter by pipeline ID")
	ciListCmd.AddCommand(listRunsCmd)

	// ── armory pipelines get run ─────────────────────────────────────────────────────

	ciGetCmd.AddCommand(&cobra.Command{
		Use:   "run <id>",
		Short: "Get a pipeline run with step details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/workflows/runs/"+args[0], nil)
		},
	})

	// ── armory pipelines update pipeline ────────────────────────────────────────────

	var (
		updatePipelineFile string
		updatePipelineName string
		updatePipelineDesc string
	)

	updatePipelineCmd := &cobra.Command{
		Use:   "pipeline <id> [dsl]",
		Short: "Update a pipeline's steps",
		Long: `Replace a pipeline's step list using a DSL string or JSON file.

  armory pipelines update pipeline <id> push->unit_tests->deploy
  armory pipelines update pipeline <id> "push->[unit_tests,security_scan]->deploy"
  armory pipelines update pipeline <id> -f pipeline.json

  # Override the name or description at the same time:
  armory pipelines update pipeline <id> push->deploy --name "slim pipeline"

The one-line DSL references stored steps by name; it cannot express inline steps,
gates, or matrices. Use -f JSON (see "create pipeline --help") to author those.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("requires <id>")
			}
			if updatePipelineFile == "" && len(args) < 2 {
				return fmt.Errorf("requires a DSL string or -f <file>")
			}
			if updatePipelineFile != "" && len(args) > 1 {
				return fmt.Errorf("cannot use both DSL string and -f")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]

			var steps []workflowStepRef
			var dslRoutes []workflowRoute
			var inputs []pipelineInputDef
			var outputs []pipelineOutputDef
			if updatePipelineFile != "" {
				data, err := os.ReadFile(updatePipelineFile)
				if err != nil {
					return fmt.Errorf("reading pipeline file: %w", err)
				}
				var pf pipelineFile
				if err := json.Unmarshal(data, &pf); err != nil {
					return fmt.Errorf("parsing pipeline file: %w", err)
				}
				steps = pf.Steps
				inputs = pf.Inputs
				outputs = pf.Outputs
				if updatePipelineName == "" {
					updatePipelineName = pf.Name
				}
				if updatePipelineDesc == "" {
					updatePipelineDesc = pf.Description
				}
				dslRoutes = pf.Routes
			} else {
				nodes, err := parseDSL(args[1])
				if err != nil {
					return err
				}
				steps, dslRoutes, err = dslToRefs(nodes)
				if err != nil {
					return err
				}
			}

			// Name is required by the API — fetch the current one if not provided.
			if updatePipelineName == "" {
				data, err := doRequest("GET", "/workflows/pipelines/"+id, nil)
				if err != nil {
					return fmt.Errorf("fetching current pipeline: %w", err)
				}
				var current struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(data, &current); err != nil || current.Name == "" {
					return fmt.Errorf("could not determine pipeline name; pass --name explicitly")
				}
				updatePipelineName = current.Name
			}

			payload := map[string]any{
				"name":  updatePipelineName,
				"steps": steps,
			}
			if len(dslRoutes) > 0 {
				payload["routes"] = dslRoutes
			}
			if mp, err := parseMapFlags(pipelineMapFlags); err != nil {
				return err
			} else if len(mp) > 0 {
				payload["maps"] = mp
			}
			if updatePipelineDesc != "" {
				payload["description"] = updatePipelineDesc
			}
			if len(inputs) > 0 {
				payload["inputs"] = inputs
			}
			if len(outputs) > 0 {
				payload["outputs"] = outputs
			}

			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/workflows/pipelines/"+id, body)
		},
	}
	updatePipelineCmd.Flags().StringVarP(&updatePipelineFile, "file", "f", "", "JSON pipeline definition file")
	updatePipelineCmd.Flags().StringArrayVar(&pipelineMapFlags, "map", nil, `define a map region the DSL refers to as [body]*<id>, as JSON (repeatable)`)
	updatePipelineCmd.Flags().StringVar(&updatePipelineName, "name", "", "Pipeline name (fetched automatically if omitted)")
	updatePipelineCmd.Flags().StringVar(&updatePipelineDesc, "description", "", "Pipeline description")
	ciUpdateCmd.AddCommand(updatePipelineCmd)

	// ── armory pipelines convert-step ────────────────────────────────────────────────

	ciCmd.AddCommand(&cobra.Command{
		Use:   "convert-step <pipeline-id> <step-name>",
		Short: "Promote an inline pipeline step into a reusable (general) step",
		Long: `Create a shared, reusable step from an inline step and repoint the pipeline
at it, so other pipelines can reference it by id. The inline step's config
becomes the new step's definition.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, stepName := args[0], args[1]
			pl, err := fetchRawPipeline(id)
			if err != nil {
				return err
			}
			idx := -1
			for i, r := range pl.StepRefs {
				if r.StepID == "" && r.Approval == nil && r.Action != "" && r.Name == stepName {
					idx = i
					break
				}
			}
			if idx < 0 {
				return fmt.Errorf("no inline step named %q in pipeline %s", stepName, id)
			}
			ref := pl.StepRefs[idx]
			stepBody, err := json.Marshal(map[string]any{
				"name": ref.Name, "action": ref.Action, "with": ref.With, "timeout": ref.Timeout,
			})
			if err != nil {
				return err
			}
			data, err := doRequest("POST", "/workflows/steps", stepBody)
			if err != nil {
				return fmt.Errorf("creating reusable step: %w", err)
			}
			var created stepDef
			if err := json.Unmarshal(data, &created); err != nil || created.StepID == "" {
				return fmt.Errorf("could not read created step id")
			}
			// Repoint the ref at the new step; keep its map region and matrix, drop the
			// now-redundant per-occurrence name (it equals the new step's own name).
			// MapID must survive: a converted step that fell out of its region would
			// stop running per value, silently changing what the pipeline does.
			pl.StepRefs[idx] = workflowStepRef{StepID: created.StepID, MapID: ref.MapID, Matrix: ref.Matrix}
			return putRawPipeline(id, pl)
		},
	})

	// ── armory pipelines localize-step ───────────────────────────────────────────────

	ciCmd.AddCommand(&cobra.Command{
		Use:   "localize-step <pipeline-id> <step-name>",
		Short: "Copy a shared step's definition inline (private to this pipeline)",
		Long: `Replace a stored-step reference with an inline copy of its definition, so
edits to the shared step no longer affect this pipeline (and vice versa).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, stepName := args[0], args[1]
			pl, err := fetchRawPipeline(id)
			if err != nil {
				return err
			}
			idx, def := -1, fullStepDef{}
			for i, r := range pl.StepRefs {
				if r.StepID == "" || r.Approval != nil {
					continue
				}
				d, err := getStepDef(r.StepID)
				if err != nil {
					return fmt.Errorf("fetching step %s: %w", r.StepID, err)
				}
				eff := r.Name
				if eff == "" {
					eff = d.Name
				}
				if eff == stepName {
					idx, def = i, d
					break
				}
			}
			if idx < 0 {
				return fmt.Errorf("no stored-step reference named %q in pipeline %s", stepName, id)
			}
			ref := pl.StepRefs[idx]
			// The inline config is the def merged with any per-occurrence override.
			merged := map[string]any{}
			maps.Copy(merged, def.With)
			maps.Copy(merged, ref.With)
			name := ref.Name
			if name == "" {
				name = def.Name
			}
			pl.StepRefs[idx] = workflowStepRef{
				Action: def.Action, Name: name, Timeout: def.Timeout, With: merged,
				MapID: ref.MapID, Matrix: ref.Matrix,
			}
			return putRawPipeline(id, pl)
		},
	})

	// ── armory pipelines cancel run ──────────────────────────────────────────────────

	ciCancelCmd.AddCommand(&cobra.Command{
		Use:   "run <id>",
		Short: "Cancel a pending, running, or awaiting-approval pipeline run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/workflows/runs/"+args[0], nil)
		},
	})

	// ── armory pipelines approve/reject run ──────────────────────────────────────────

	ciApproveCmd.AddCommand(&cobra.Command{
		Use:   "run <id>",
		Short: "Approve a run paused on a manual-approval gate, resuming it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("POST", "/workflows/runs/"+args[0]+"/approve", nil)
		},
	})
	ciRejectCmd.AddCommand(&cobra.Command{
		Use:   "run <id>",
		Short: "Reject a run paused on a manual-approval gate, failing it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("POST", "/workflows/runs/"+args[0]+"/reject", nil)
		},
	})

	// ── armory pipelines list actions ────────────────────────────────────────────────

	ciListCmd.AddCommand(&cobra.Command{
		Use:   "actions",
		Short: "List available workflow actions from the service catalog",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/workflows/actions", nil) },
	})

	// ── armory pipelines test step ───────────────────────────────────────────────────

	ciTestCmd := &cobra.Command{Use: "test", Short: "Test workflow building blocks in isolation"}
	var testStepInputs []string
	testStepCmd := &cobra.Command{
		Use:   "step <id>",
		Short: "Run a single step in isolation (throwaway pipeline) to test it before composing",
		Long: `Run one step by itself to test it before adding it to a pipeline.

The step runs through a throwaway single-step pipeline that is deleted afterward.
Supply a value for each ${...} reference the step uses with --input:

  armory pipelines test step <id> \
    --input inputs.ENV=staging \
    --input steps.build.output='build ok'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := doRequest("GET", "/workflows/steps/"+args[0], nil)
			if err != nil {
				return fmt.Errorf("fetching step: %w", err)
			}
			var step struct {
				Name    string         `json:"name"`
				Action  string         `json:"action"`
				With    map[string]any `json:"with"`
				Timeout int64          `json:"timeout"`
			}
			if err := json.Unmarshal(data, &step); err != nil {
				return fmt.Errorf("parsing step: %w", err)
			}

			vals := map[string]string{}
			for _, kv := range testStepInputs {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("invalid --input %q: expected ref=value", kv)
				}
				vals[k] = v
			}
			for _, ref := range stepWithRefs(step.With) {
				if _, ok := vals[ref]; !ok {
					fmt.Printf("note: no value supplied for ${%s}; leaving it unresolved\n", ref)
				}
			}

			outcome, err := runStepTest(step.Name, step.Action, resolveStepWith(step.With, vals), step.Timeout)
			if err != nil {
				return err
			}
			fmt.Printf("status: %s\n", outcome.status)
			if out := strings.TrimSpace(outcome.output); out != "" {
				fmt.Printf("\noutput:\n%s\n", out)
			}
			if outcome.status != "completed" {
				return fmt.Errorf("step test did not complete (status: %s)", outcome.status)
			}
			return nil
		},
	}
	testStepCmd.Flags().StringArrayVar(&testStepInputs, "input", nil, "Test value for a ${ref}, as ref=value (repeatable)")
	ciTestCmd.AddCommand(testStepCmd)

	ciCmd.AddCommand(ciCreateCmd, ciListCmd, ciGetCmd, ciUpdateCmd, ciDeleteCmd, ciRunCmd, ciCancelCmd, ciApproveCmd, ciRejectCmd, ciTestCmd, ciTUICmd, stepsTUICmd)
	// The workflows module (ci command + the CI/Pipelines and Steps home
	// screens) is registered in ci_tui.go.
}

// buildWith constructs the step With map from CLI flags.
// For forge/run, --image, --run, --env, and --runner-class are convenience flags
// merged into any --with JSON base. For all other actions, --with JSON is required.
func buildWith(action, withJSON, image, run, runnerClass string, envs []string) (map[string]any, error) {
	with := map[string]any{}

	if withJSON != "" {
		if err := json.Unmarshal([]byte(withJSON), &with); err != nil {
			return nil, fmt.Errorf("--with is not valid JSON: %w", err)
		}
	}

	if action == "forge/run" {
		if image != "" {
			with["image"] = image
		}
		if run != "" {
			with["run"] = run
		}
		if runnerClass != "" {
			with["runner_class"] = runnerClass
		}
		if len(envs) > 0 {
			env := map[string]string{}
			for _, kv := range envs {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return nil, fmt.Errorf("invalid --env %q: expected KEY=VALUE", kv)
				}
				env[k] = v
			}
			with["env"] = env
		}
		if _, ok := with["image"]; !ok {
			return nil, fmt.Errorf("forge/run requires --image or \"image\" in --with JSON")
		}
		if _, hasRun := with["run"]; !hasRun {
			if _, hasCmd := with["command"]; !hasCmd {
				return nil, fmt.Errorf("forge/run requires --run or \"run\"/\"command\" in --with JSON")
			}
		}
		return with, nil
	}

	if withJSON == "" {
		return nil, fmt.Errorf("--with <json> is required for action %q", action)
	}
	return with, nil
}
