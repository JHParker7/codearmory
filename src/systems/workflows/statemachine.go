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

// smState is one state, of exactly one KIND:
//   - a task (Run an inline action, or Use a stored step) — does work, then flows on
//     via Next (one target, or a list for a parallel fan-out) or End;
//   - an Approval gate — a manual pause, likewise Next/End;
//   - a Map region — a nested subgraph repeated per value;
//   - a Choice — a pure decision that only routes: it does NO work, and instead of
//     Next carries Choice branches. Keeping the decision OUT of the runner states
//     (as a real state machine does) is deliberate: a step that both runs and
//     branches is the thing that reads confusingly.
//
// So Choice is mutually exclusive with Run/Use/Approval/Map and with Next/End; a
// task/approval/map state must NOT carry Choice.
type smState struct {
	// One kind:
	Run      string         `json:"run,omitempty"`  // inline step: the action it runs
	Use      string         `json:"use,omitempty"`  // reference: a stored step_id
	Approval *ApprovalGate  `json:"approval,omitempty"`
	Map      *smMap         `json:"map,omitempty"`
	Choice   []smChoice     `json:"choice,omitempty"` // decision kind — no Run/Next
	// Task modifiers (Run/Use only):
	With    map[string]any `json:"with,omitempty"`
	Timeout int64          `json:"timeout,omitempty"`
	Matrix  *MatrixConfig  `json:"matrix,omitempty"`
	Scatter *ScatterConfig `json:"scatter,omitempty"`
	// Permissions the run role needs for this state — carried so the state-machine
	// shape is a LOSSLESS view of the step. Omitting it was not cosmetic: every GET
	// returns a computed state_machine, so a client that read a pipeline and wrote it
	// back had its declared grants silently dropped, the run role was re-minted
	// without them, and the next run failed with a 403 that pointed at permissions
	// nobody had changed.
	Permissions []PermissionSpec `json:"permissions,omitempty"`
	// AllowUnresolved is the step's unresolved-reference opt-out, carried for exactly
	// the reason Permissions is: the state-machine shape must be a LOSSLESS view of the
	// step. Dropped, a GET-edit-PUT round-trip silently re-armed the strict check on a
	// step that legitimately passes a literal ${...} through to its action, and the next
	// run failed the step with an unresolved-reference error on config nobody touched.
	AllowUnresolved bool `json:"allow_unresolved,omitempty"`
	// Transition for the non-Choice kinds:
	Next smNext `json:"next,omitempty"`
	End  bool   `json:"end,omitempty"`
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
	// Name is an optional label for this branch, shown instead of the condition.
	Name string `json:"name,omitempty"`
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

// smToModel converts a state-machine document into the stored triple.
//
// A CHOICE state is not a node: it is a decision that lowers to the conditional
// routes leaving whatever step flows INTO it — so a runner step never carries the
// branch itself. A MAP container is not a node either; it resolves to its member
// sub-states, with the region boundary rewired (an edge into the container → edges
// into the region's entry sub-states; the container's own transition → edges out of
// its exit sub-states). Only structural checks happen here; cycles, unknown steps
// and region rules are left to the existing validateGraph chain.
func smToModel(doc *smDoc) (steps []WorkflowStepRef, routes []WorkflowRoute, maps []MapDef, err error) {
	if doc == nil || len(doc.States) == 0 {
		return nil, nil, nil, fmt.Errorf("state machine has no states")
	}

	// region records how a map container resolves at its boundary.
	type region struct {
		entries []string // sub-states with no inbound edge from within the region
		exits   []string // sub-states that terminate the region (End or no outbound)
	}
	// branch is one lowered choice arm: a target and the condition that selects it.
	type branch struct {
		to   string
		when string
		name string
	}
	regions := map[string]*region{}
	choices := map[string][]branch{} // choice state name -> its arms
	stepNodes := map[string]bool{}   // names that ARE backend steps (not choices/maps)
	seenNames := map[string]bool{}   // every state/sub-state name, for uniqueness

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

	// isChoiceState reports a pure decision (Choice set, nothing else) and validates
	// that a choice carries no work / no plain transition.
	isChoiceState := func(name string, st *smState) (bool, error) {
		if len(st.Choice) == 0 {
			return false, nil
		}
		if st.Run != "" || st.Use != "" || st.Approval != nil || st.Map != nil {
			return false, fmt.Errorf("choice state %q cannot also run a step — a choice only routes", name)
		}
		if len(st.Next) > 0 || st.End {
			return false, fmt.Errorf("choice state %q cannot set next/end — put targets in its branches", name)
		}
		return true, nil
	}

	// taskRef builds the WorkflowStepRef for a Run/Use/Approval state (not a map/choice).
	taskRef := func(name string, st *smState, mapID string) (WorkflowStepRef, error) {
		if len(st.Choice) > 0 {
			return WorkflowStepRef{}, fmt.Errorf("state %q sets choice but also a step kind", name)
		}
		ref := WorkflowStepRef{Name: name, With: st.With, MapID: mapID, Permissions: st.Permissions, AllowUnresolved: st.AllowUnresolved}
		switch {
		case st.Approval != nil:
			ref.Approval = st.Approval
		case st.Use != "":
			ref.StepID = st.Use
		case st.Run != "":
			ref.Action = st.Run
			ref.Timeout = st.Timeout
		default:
			return ref, fmt.Errorf("state %q must set one of run, use, approval, map or choice", name)
		}
		if st.Matrix != nil {
			ref.Matrix = st.Matrix
		}
		if st.Scatter != nil {
			ref.Scatter = st.Scatter
		}
		return ref, nil
	}

	// nextTargets validates and returns a non-choice state's plain transition targets.
	nextTargets := func(name string, st *smState) ([]string, error) {
		if st.End && len(st.Next) > 0 {
			return nil, fmt.Errorf("state %q sets end together with next", name)
		}
		return []string(st.Next), nil
	}

	// Pass 1a — record choice states (they are not nodes).
	for name, st := range doc.States {
		choice, err := isChoiceState(name, st)
		if err != nil {
			return nil, nil, nil, err
		}
		if !choice {
			continue
		}
		if err := claim(name); err != nil {
			return nil, nil, nil, err
		}
		arms := make([]branch, 0, len(st.Choice))
		for _, c := range st.Choice {
			switch {
			case c.Default != "":
				arms = append(arms, branch{to: c.Default, name: c.Name}) // else: no `when`
			case c.Next != "":
				arms = append(arms, branch{to: c.Next, when: c.When, name: c.Name})
			default:
				return nil, nil, nil, fmt.Errorf("choice state %q: a branch needs next or default", name)
			}
		}
		choices[name] = arms
	}

	// Pass 1b — register map regions and their sub-states, computing internal routes
	// and each region's entry/exit sub-states. (A map body is task states only.)
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
		// A map body may itself use choice states (a per-value iteration that branches),
		// lowered to conditional internal routes exactly as at the top level.
		subChoices := map[string][]branch{}
		regionStep := map[string]bool{}
		for subName, sub := range m.States {
			if sub.Map != nil {
				return nil, nil, nil, fmt.Errorf("map %q: nested maps are not supported", name)
			}
			choice, cerr := isChoiceState(subName, sub)
			if cerr != nil {
				return nil, nil, nil, cerr
			}
			if !choice {
				continue
			}
			if err := claim(subName); err != nil {
				return nil, nil, nil, err
			}
			arms := make([]branch, 0, len(sub.Choice))
			for _, c := range sub.Choice {
				switch {
				case c.Default != "":
					arms = append(arms, branch{to: c.Default, name: c.Name})
				case c.Next != "":
					arms = append(arms, branch{to: c.Next, when: c.When, name: c.Name})
				default:
					return nil, nil, nil, fmt.Errorf("map %q: choice %q needs next or default", name, subName)
				}
			}
			subChoices[subName] = arms
		}
		for subName, sub := range m.States {
			if _, isCh := subChoices[subName]; isCh {
				continue
			}
			if err := claim(subName); err != nil {
				return nil, nil, nil, err
			}
			ref, rerr := taskRef(subName, sub, name)
			if rerr != nil {
				return nil, nil, nil, rerr
			}
			steps = append(steps, ref)
			regionStep[subName] = true
		}
		resolveRegion := func(t string) (string, error) {
			if regionStep[t] {
				return t, nil
			}
			if _, ok := subChoices[t]; ok {
				return "", fmt.Errorf("map %q: a choice cannot route to another choice (%q)", name, t)
			}
			return "", fmt.Errorf("map %q: state %q is not in the region", name, t)
		}
		for subName, sub := range m.States {
			if _, isCh := subChoices[subName]; isCh {
				continue
			}
			tgts, terr := nextTargets(subName, sub)
			if terr != nil {
				return nil, nil, nil, terr
			}
			if len(tgts) == 0 {
				reg.exits = append(reg.exits, subName) // End or no transition
			}
			for _, raw := range tgts {
				if arms, ok := subChoices[raw]; ok {
					for _, a := range arms {
						d, derr := resolveRegion(a.to)
						if derr != nil {
							return nil, nil, nil, derr
						}
						internalRoutes = append(internalRoutes, WorkflowRoute{From: subName, To: d, When: a.when, Name: a.name})
						hasInbound[d] = true
					}
					continue
				}
				d, derr := resolveRegion(raw)
				if derr != nil {
					return nil, nil, nil, derr
				}
				internalRoutes = append(internalRoutes, WorkflowRoute{From: subName, To: d})
				hasInbound[d] = true
			}
		}
		for subName := range regionStep {
			if !hasInbound[subName] {
				reg.entries = append(reg.entries, subName)
			}
		}
		sort.Strings(reg.entries)
		sort.Strings(reg.exits)
		regions[name] = reg
	}

	// Pass 1c — register top-level task/approval states as step nodes.
	for name, st := range doc.States {
		if st.Map != nil || len(st.Choice) > 0 {
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
		stepNodes[name] = true
	}

	// resolveTargets expands a NON-choice raw target: a map container to its entry
	// sub-states, a step node to itself.
	resolveTargets := func(name string) ([]string, error) {
		if reg, ok := regions[name]; ok {
			if len(reg.entries) == 0 {
				return nil, fmt.Errorf("map %q has no entry state (its states form a cycle)", name)
			}
			return reg.entries, nil
		}
		if !stepNodes[name] {
			if _, ok := choices[name]; ok {
				return nil, fmt.Errorf("a choice cannot route directly to another choice (%q)", name)
			}
			return nil, fmt.Errorf("transition to unknown state %q", name)
		}
		return []string{name}, nil
	}

	// Pass 2 — routes. Each source (a step, or a map container via its exits) flows to
	// its Next targets; a target that is a CHOICE lowers to that choice's conditional
	// arms, so the branch attaches to the SOURCE step, never a runner state of its own.
	routes = append(routes, internalRoutes...)
	seenRoute := map[WorkflowRoute]bool{}
	for _, r := range routes {
		seenRoute[r] = true
	}
	addRoute := func(from, to, when, name string) {
		r := WorkflowRoute{From: from, To: to, When: when, Name: name}
		if !seenRoute[r] {
			seenRoute[r] = true
			routes = append(routes, r)
		}
	}
	reachedChoice := map[string]bool{}
	for name, st := range doc.States {
		if len(st.Choice) > 0 {
			continue // a choice emits no routes of its own; its predecessors do
		}
		tgts, err := nextTargets(name, st)
		if err != nil {
			return nil, nil, nil, err
		}
		var sources []string
		if reg, ok := regions[name]; ok {
			sources = reg.exits
		} else {
			sources = []string{name}
		}
		for _, raw := range tgts {
			if arms, ok := choices[raw]; ok {
				reachedChoice[raw] = true
				for _, a := range arms {
					dests, err := resolveTargets(a.to)
					if err != nil {
						return nil, nil, nil, err
					}
					for _, src := range sources {
						for _, d := range dests {
							addRoute(src, d, a.when, a.name)
						}
					}
				}
				continue
			}
			dests, err := resolveTargets(raw)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, src := range sources {
				for _, d := range dests {
					addRoute(src, d, "", "")
				}
			}
		}
	}
	for name := range choices {
		if !reachedChoice[name] {
			return nil, nil, nil, fmt.Errorf("choice state %q is unreachable — a step must route to it", name)
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
	type edge struct{ to, when, name string }
	outer := map[string][]edge{}
	internal := map[string][]edge{}
	inboundInternal := map[string]bool{}
	for _, r := range routes {
		fm, tm := mapOf[r.From], mapOf[r.To]
		if fm != "" && fm == tm {
			internal[r.From] = append(internal[r.From], edge{r.To, r.When, r.Name})
			inboundInternal[r.To] = true
			continue
		}
		outer[display(r.From)] = append(outer[display(r.From)], edge{display(r.To), r.When, r.Name})
	}

	// used tracks every taken name, so a synthetic choice-state name never collides.
	used := map[string]bool{}
	for _, s := range steps {
		used[s.Name] = true
	}
	for _, m := range maps {
		used[m.ID] = true
	}
	uniqueChoiceName := func(base string) string {
		n := base + "_choice"
		for i := 2; used[n]; i++ {
			n = fmt.Sprintf("%s_choice%d", base, i)
		}
		used[n] = true
		return n
	}

	// apply lowers a node's outbound edges onto its state. Unconditional edges become
	// Next (one target, or a list for parallelism); the moment any edge is conditional
	// the branch is split OUT into its own Choice state (emitted via emit), so the
	// runner state only ever points at the decision — never carries it.
	apply := func(node string, edges []edge, st *smState, emit func(name string, s *smState)) {
		if len(edges) == 0 {
			st.End = true
			return
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
			st.Next = ns
			return
		}
		cname := uniqueChoiceName(node)
		st.Next = smNext{cname}
		cs := make([]smChoice, 0, len(edges))
		for _, e := range edges { // when-branches first, defaults last
			if e.when != "" {
				cs = append(cs, smChoice{When: e.when, Next: e.to, Name: e.name})
			}
		}
		for _, e := range edges {
			if e.when == "" {
				cs = append(cs, smChoice{Default: e.to, Name: e.name})
			}
		}
		emit(cname, &smState{Choice: cs})
	}

	taskState := func(s WorkflowStep) *smState {
		st := &smState{With: s.With, Matrix: s.Matrix, Scatter: s.Scatter, Approval: s.Approval, Permissions: s.Permissions, AllowUnresolved: s.AllowUnresolved}
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
	for _, m := range maps {
		doc.States[m.ID] = &smState{Map: &smMap{
			Var: m.Var, Values: m.Values, Over: m.ValuesFrom,
			MaxConcurrent: m.MaxConcurrent, Sequential: m.Sequential,
			Volume: m.Volume, MountPath: m.MountPath, SizeMB: m.SizeMB, Medium: m.Medium, Outputs: m.Outputs,
			States: map[string]*smState{},
		}}
	}
	for _, s := range steps {
		st := taskState(s)
		if s.MapID != "" {
			container := doc.States[s.MapID]
			if container == nil || container.Map == nil {
				continue
			}
			// A conditional edge inside the region emits a choice SUB-state.
			apply(s.Name, internal[s.Name], st, func(n string, cs *smState) { container.Map.States[n] = cs })
			container.Map.States[s.Name] = st
			continue
		}
		apply(s.Name, outer[s.Name], st, func(n string, cs *smState) { doc.States[n] = cs })
		doc.States[s.Name] = st
	}
	// Map containers' own transitions come from their outer edges.
	for _, m := range maps {
		if c := doc.States[m.ID]; c != nil {
			apply(m.ID, outer[m.ID], c, func(n string, cs *smState) { doc.States[n] = cs })
		}
	}
	return doc
}
