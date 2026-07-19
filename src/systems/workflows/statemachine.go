package main

// State-machine pipeline document ⇄ internal model.
//
// The service stores a pipeline as (StepRefs, Routes, Maps, Inputs, Outputs) — a
// DAG of named nodes with explicit edges. That is precise but noisy to author by
// hand. This file adds a second, human-authored representation — a *state machine*
// in the Amazon-States-Language style — and the pure conversion both directions:
//
//	smToModel   state-machine document -> (steps, routes, maps)
//	modelToSM   (steps, routes, maps)  -> state-machine document
//
// The document is plain JSON (these structs carry json tags only). YAML is a
// client-side presentation concern — every caller converts YAML⇆JSON before it
// reaches the API — so the service needs no YAML dependency. The converter runs
// BEFORE the existing validation chain (validateStepRefs → enrichStepRefs →
// validateGraph), so cycles, unknown steps and bad map regions are still caught by
// the current code, and nothing downstream of the request struct changes.
//
// The document names each state; the state key IS the node name that routes and
// ${steps.<name>.output} already use, so the two models share one identity space.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// smDoc is a whole pipeline as a state machine. States is a map keyed by state
// name; ordering is NOT taken from the map (JSON objects are unordered) — the
// converter emits steps in a deterministic topological order derived from the
// routes, so a round-trip is stable regardless of key order.
type smDoc struct {
	Name        string              `json:"name,omitempty"`
	Description string              `json:"description,omitempty"`
	Inputs      []WorkflowInputDef  `json:"inputs,omitempty"`
	Outputs     []WorkflowOutputDef `json:"outputs,omitempty"`
	States      map[string]*smState `json:"states"`
}

// smState is one state. Exactly one "kind" is set: Run (an inline action), Use (a
// reference to a stored step by id), Approval (a manual gate), or Map (a region — a
// nested subgraph repeated per value). Matrix/Scatter fan the single Run/Use state
// out and are mutually exclusive with Map. Exactly one transition is set: Next (one
// target, or a list for a parallel fan-out), Choice (conditional branches), or End.
type smState struct {
	// One kind:
	Run      string         `json:"run,omitempty"`  // inline step: the action it runs
	Use      string         `json:"use,omitempty"`  // reference: a stored step_id
	Approval *ApprovalGate  `json:"approval,omitempty"`
	Map      *smMap         `json:"map,omitempty"`
	// Task modifiers (Run/Use only):
	With    map[string]any `json:"with,omitempty"`
	Timeout int64          `json:"timeout,omitempty"`
	Matrix  *MatrixConfig  `json:"matrix,omitempty"`
	Scatter *ScatterConfig `json:"scatter,omitempty"`
	// One transition:
	Next   smNext     `json:"next,omitempty"`
	Choice []smChoice `json:"choice,omitempty"`
	End    bool       `json:"end,omitempty"`
}

// smMap is a map region embedded in a state. Over maps to MapDef.ValuesFrom; the
// region's body is States (its own sub-graph of task states), repeated per value.
type smMap struct {
	Var           string              `json:"var"`
	Values        []string            `json:"values,omitempty"`
	Over          string              `json:"over,omitempty"` // -> MapDef.ValuesFrom
	MaxConcurrent int                 `json:"max_concurrent,omitempty"`
	Sequential    bool                `json:"sequential,omitempty"`
	Volume        string              `json:"volume,omitempty"`
	MountPath     string              `json:"mount_path,omitempty"`
	SizeMB        int64               `json:"size_mb,omitempty"`
	Medium        string              `json:"medium,omitempty"`
	Outputs       []string            `json:"outputs,omitempty"`
	States        map[string]*smState `json:"states"`
}

// smChoice is one conditional branch out of a Choice state. Exactly one of Next
// (taken when When holds) or Default (the else — taken unconditionally on completion,
// i.e. a route with no `when`) is set.
type smChoice struct {
	When    string `json:"when,omitempty"`
	Next    string `json:"next,omitempty"`
	Default string `json:"default,omitempty"`
}

// smNext is a transition target that accepts either a single state name ("next: x")
// or a list ("next: [a, b]") for a parallel fan-out. It always marshals back to the
// compact form: a bare string for one target, a list for several.
type smNext []string

func (n *smNext) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*n = smNext{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("next: expected a state name or a list of names")
	}
	*n = many
	return nil
}

func (n smNext) MarshalJSON() ([]byte, error) {
	if len(n) == 1 {
		return json.Marshal(n[0])
	}
	return json.Marshal([]string(n))
}

// ── document -> internal model ──────────────────────────────────────────────────

// smToModel converts a state-machine document into the stored triple. It resolves
// map containers (which are NOT nodes themselves) into their member sub-states and
// rewires the region boundary: an edge INTO a container becomes edges into the
// region's entry sub-states, and a container's own transition becomes edges out of
// the region's exit sub-states. It performs only structural checks (a state names a
// kind, transitions resolve, names are unique); semantic validation — cycles,
// unknown steps, region rules — is left to the existing validateGraph chain.
func smToModel(doc *smDoc) (steps []WorkflowStepRef, routes []WorkflowRoute, maps []MapDef, err error) {
	if doc == nil || len(doc.States) == 0 {
		return nil, nil, nil, fmt.Errorf("state machine has no states")
	}

	// region records how a map container resolves at its boundary.
	type region struct {
		entries []string // sub-states with no inbound edge from within the region
		exits   []string // sub-states that terminate the region (End or no outbound)
	}
	regions := map[string]*region{}
	seenNames := map[string]bool{}

	claim := func(name string) error {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("a state name cannot be empty")
		}
		if seenNames[name] {
			return fmt.Errorf("duplicate state name %q", name)
		}
		seenNames[name] = true
		return nil
	}

	// taskRef builds the WorkflowStepRef for a Run/Use/Approval state (not a map).
	taskRef := func(name string, st *smState, mapID string) (WorkflowStepRef, error) {
		ref := WorkflowStepRef{Name: name, With: st.With, MapID: mapID}
		switch {
		case st.Approval != nil:
			ref.Approval = st.Approval
		case st.Use != "":
			ref.StepID = st.Use
		case st.Run != "":
			ref.Action = st.Run
			ref.Timeout = st.Timeout
		default:
			return ref, fmt.Errorf("state %q must set one of run, use, approval or map", name)
		}
		if st.Matrix != nil {
			ref.Matrix = st.Matrix
		}
		if st.Scatter != nil {
			ref.Scatter = st.Scatter
		}
		return ref, nil
	}

	// transitionEdges turns a state's Next/Choice into (target,when) pairs. Targets
	// are raw names here (possibly map containers); they are resolved to concrete
	// nodes by the caller via resolveTargets.
	type edge struct {
		to   string
		when string
	}
	transitionEdges := func(name string, st *smState) ([]edge, error) {
		hasNext, hasChoice := len(st.Next) > 0, len(st.Choice) > 0
		if st.End && (hasNext || hasChoice) {
			return nil, fmt.Errorf("state %q sets end together with a transition", name)
		}
		if hasNext && hasChoice {
			return nil, fmt.Errorf("state %q sets both next and choice", name)
		}
		var edges []edge
		for _, t := range st.Next {
			edges = append(edges, edge{to: t})
		}
		for _, c := range st.Choice {
			switch {
			case c.Default != "":
				edges = append(edges, edge{to: c.Default}) // else: no `when`
			case c.Next != "":
				edges = append(edges, edge{to: c.Next, when: c.When})
			default:
				return nil, fmt.Errorf("state %q: a choice branch needs next or default", name)
			}
		}
		return edges, nil
	}

	// Pass 1 — register every node (top-level task states and map sub-states) and,
	// for each map, compute its internal routes plus its entry/exit sub-states.
	var internalRoutes []WorkflowRoute
	for name, st := range doc.States {
		if st.Map == nil {
			continue
		}
		m := st.Map
		if len(m.States) == 0 {
			return nil, nil, nil, fmt.Errorf("map %q has no states", name)
		}
		if err := claim(name); err != nil { // the container name is reserved (not a node)
			return nil, nil, nil, err
		}
		maps = append(maps, MapDef{
			ID: name, Var: m.Var, Values: m.Values, ValuesFrom: m.Over,
			MaxConcurrent: m.MaxConcurrent, Sequential: m.Sequential,
			Volume: m.Volume, MountPath: m.MountPath, SizeMB: m.SizeMB, Medium: m.Medium, Outputs: m.Outputs,
		})
		reg := &region{}
		hasInbound := map[string]bool{}
		for subName, sub := range m.States {
			if sub.Map != nil {
				return nil, nil, nil, fmt.Errorf("map %q: nested maps are not supported", name)
			}
			if err := claim(subName); err != nil {
				return nil, nil, nil, err
			}
			ref, err := taskRef(subName, sub, name)
			if err != nil {
				return nil, nil, nil, err
			}
			steps = append(steps, ref)
			edges, err := transitionEdges(subName, sub)
			if err != nil {
				return nil, nil, nil, err
			}
			if len(edges) == 0 {
				reg.exits = append(reg.exits, subName) // End or no transition
			}
			for _, e := range edges {
				if _, ok := m.States[e.to]; !ok {
					return nil, nil, nil, fmt.Errorf("map %q: state %q routes to %q which is not in the region", name, subName, e.to)
				}
				internalRoutes = append(internalRoutes, WorkflowRoute{From: subName, To: e.to, When: e.when})
				hasInbound[e.to] = true
			}
		}
		for subName := range m.States {
			if !hasInbound[subName] {
				reg.entries = append(reg.entries, subName)
			}
		}
		sort.Strings(reg.entries)
		sort.Strings(reg.exits)
		regions[name] = reg
	}
	for name, st := range doc.States {
		if st.Map != nil {
			continue
		}
		if err := claim(name); err != nil {
			return nil, nil, nil, err
		}
		ref, err := taskRef(name, st, "")
		if err != nil {
			return nil, nil, nil, err
		}
		steps = append(steps, ref)
	}

	// resolveTargets expands a raw transition target: a map container resolves to
	// its entry sub-states, any other name to itself.
	resolveTargets := func(name string) ([]string, error) {
		if reg, ok := regions[name]; ok {
			if len(reg.entries) == 0 {
				return nil, fmt.Errorf("map %q has no entry state (its states form a cycle)", name)
			}
			return reg.entries, nil
		}
		if !seenNames[name] {
			return nil, fmt.Errorf("transition to unknown state %q", name)
		}
		return []string{name}, nil
	}

	// Pass 2 — outer transitions. A task state emits edges from itself; a map
	// container emits from each of its exit sub-states.
	routes = append(routes, internalRoutes...)
	seenRoute := map[WorkflowRoute]bool{}
	for _, r := range routes {
		seenRoute[r] = true
	}
	addRoute := func(from, to, when string) {
		r := WorkflowRoute{From: from, To: to, When: when}
		if !seenRoute[r] {
			seenRoute[r] = true
			routes = append(routes, r)
		}
	}
	for name, st := range doc.States {
		edges, err := transitionEdges(name, st)
		if err != nil {
			return nil, nil, nil, err
		}
		var sources []string
		if reg, ok := regions[name]; ok {
			sources = reg.exits
		} else {
			sources = []string{name}
		}
		for _, e := range edges {
			targets, err := resolveTargets(e.to)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, src := range sources {
				for _, tgt := range targets {
					addRoute(src, tgt, e.when)
				}
			}
		}
	}

	steps = orderSteps(steps, routes)
	if len(routes) == 0 {
		routes = nil // a pure sequence: let the engine derive the chain
	}
	return steps, routes, maps, nil
}

// orderSteps returns the step refs in a deterministic, readable order: a topological
// sort by the routes (entries first) with alphabetical tie-breaking, so a document
// round-trips stably. A residual cycle (rejected later by validateGraph) falls back
// to appending the remaining refs in name order.
func orderSteps(steps []WorkflowStepRef, routes []WorkflowRoute) []WorkflowStepRef {
	byName := map[string]WorkflowStepRef{}
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	sort.Strings(names)
	indeg := map[string]int{}
	adj := map[string][]string{}
	for _, n := range names {
		indeg[n] = 0
	}
	for _, r := range routes {
		if _, ok := byName[r.From]; !ok {
			continue
		}
		if _, ok := byName[r.To]; !ok {
			continue
		}
		adj[r.From] = append(adj[r.From], r.To)
		indeg[r.To]++
	}
	var queue []string
	for _, n := range names { // names is sorted → deterministic tie-break
		if indeg[n] == 0 {
			queue = append(queue, n)
		}
	}
	var out []WorkflowStepRef
	emitted := map[string]bool{}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		out = append(out, byName[n])
		emitted[n] = true
		nexts := append([]string(nil), adj[n]...)
		sort.Strings(nexts)
		for _, m := range nexts {
			indeg[m]--
			if indeg[m] == 0 {
				// insert keeping the queue sorted enough; simplest: append then it is
				// picked in FIFO — good enough since we seeded sorted and sort nexts.
				queue = append(queue, m)
			}
		}
	}
	for _, n := range names { // any residual (cycle) in name order
		if !emitted[n] {
			out = append(out, byName[n])
		}
	}
	return out
}

// ── internal model -> document ──────────────────────────────────────────────────

// modelToSM builds the state-machine document for a stored pipeline, so a client can
// display and edit it as a state machine. It is the inverse of smToModel: map members
// become a container's nested states, boundary edges become the container's inbound
// target / outbound transition, and each node's outbound routes become a Next (one or
// many) or a Choice.
func modelToSM(name, description string, steps []WorkflowStep, routes []WorkflowRoute, maps []MapDef, inputs []WorkflowInputDef, outputs []WorkflowOutputDef) *smDoc {
	mapOf := map[string]string{} // step name -> map id it belongs to
	for _, s := range steps {
		if s.MapID != "" {
			mapOf[s.Name] = s.MapID
		}
	}
	// displayName folds a map member to its container for OUTER edges.
	display := func(n string) string {
		if id, ok := mapOf[n]; ok {
			return id
		}
		return n
	}
	// Partition routes: internal (both ends in the same region) vs outer.
	type edge struct{ to, when string }
	outer := map[string][]edge{}
	internal := map[string][]edge{}
	inboundInternal := map[string]bool{}
	for _, r := range routes {
		fm, tm := mapOf[r.From], mapOf[r.To]
		if fm != "" && fm == tm {
			internal[r.From] = append(internal[r.From], edge{r.To, r.When})
			inboundInternal[r.To] = true
			continue
		}
		outer[display(r.From)] = append(outer[display(r.From)], edge{display(r.To), r.When})
	}

	transition := func(edges []edge) (smNext, []smChoice, bool) {
		if len(edges) == 0 {
			return nil, nil, true // end
		}
		conditional := false
		for _, e := range edges {
			if e.when != "" {
				conditional = true
			}
		}
		if !conditional {
			ns := make(smNext, 0, len(edges))
			for _, e := range edges {
				ns = append(ns, e.to)
			}
			sort.Strings(ns)
			return ns, nil, false
		}
		var cs []smChoice
		for _, e := range edges { // when-branches first, defaults last
			if e.when != "" {
				cs = append(cs, smChoice{When: e.when, Next: e.to})
			}
		}
		for _, e := range edges {
			if e.when == "" {
				cs = append(cs, smChoice{Default: e.to})
			}
		}
		return nil, cs, false
	}

	taskState := func(s WorkflowStep) *smState {
		st := &smState{With: s.With, Matrix: s.Matrix, Scatter: s.Scatter, Approval: s.Approval}
		switch {
		case s.Approval != nil:
			// gate: no run/use
		case s.StepID != "" && s.Action == "": // pure reference
			st.Use = s.StepID
		default:
			st.Run = s.Action
			st.Timeout = s.Timeout
		}
		if len(st.With) == 0 {
			st.With = nil
		}
		return st
	}

	doc := &smDoc{Name: name, Description: description, Inputs: inputs, Outputs: outputs, States: map[string]*smState{}}
	// Map containers first, with their nested states.
	byID := map[string]MapDef{}
	for _, m := range maps {
		byID[m.ID] = m
	}
	for _, m := range maps {
		sm := &smMap{
			Var: m.Var, Values: m.Values, Over: m.ValuesFrom,
			MaxConcurrent: m.MaxConcurrent, Sequential: m.Sequential,
			Volume: m.Volume, MountPath: m.MountPath, SizeMB: m.SizeMB, Medium: m.Medium, Outputs: m.Outputs,
			States: map[string]*smState{},
		}
		doc.States[m.ID] = &smState{Map: sm}
	}
	for _, s := range steps {
		st := taskState(s)
		if s.MapID != "" {
			// sub-state: transition comes from internal edges
			next, choice, end := transition(internal[s.Name])
			st.Next, st.Choice, st.End = next, choice, end
			if container, ok := doc.States[s.MapID]; ok && container.Map != nil {
				container.Map.States[s.Name] = st
			}
			continue
		}
		next, choice, end := transition(outer[s.Name])
		st.Next, st.Choice, st.End = next, choice, end
		doc.States[s.Name] = st
	}
	// Map containers' own transitions come from their outer edges.
	for _, m := range maps {
		next, choice, end := transition(outer[m.ID])
		if c := doc.States[m.ID]; c != nil {
			c.Next, c.Choice, c.End = next, choice, end
		}
	}
	return doc
}
