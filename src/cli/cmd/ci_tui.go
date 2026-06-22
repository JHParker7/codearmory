package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// ── Styles ────────────────────────────────────────────────────────────────────

// These styles are shared by every list/detail TUI (ci, forge, audit, steps,
// hooks). buildCITUIStyles rebuilds them, plus the status color map, from the
// active theme.
var (
	tuiBoxStyle   lipgloss.Style
	tuiTitleStyle lipgloss.Style
	tuiMetaStyle  lipgloss.Style
	tuiHelpStyle  lipgloss.Style
	tuiErrStyle   lipgloss.Style

	// Pipeline flow-diagram styles: a sequential step is a plain bordered box,
	// a parallel stage is accent-bordered, and stages are joined by muted arrows.
	tuiDiagStepStyle  lipgloss.Style
	tuiDiagParStyle   lipgloss.Style
	tuiDiagArrowStyle lipgloss.Style

	tuiStatusColors map[string]lipgloss.Color
)

func buildCITUIStyles() {
	tuiBoxStyle = lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(activeTheme.Border))

	tuiTitleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(activeTheme.Accent))

	tuiMetaStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	tuiHelpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	tuiErrStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Danger))

	tuiDiagStepStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(activeTheme.Border)).
		Padding(0, 1)
	tuiDiagParStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(activeTheme.Accent)).
		Padding(0, 1)
	tuiDiagArrowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))

	tuiStatusColors = map[string]lipgloss.Color{
		"completed": lipgloss.Color(activeTheme.Accent),
		"running":   lipgloss.Color(activeTheme.Warning),
		"pending":   lipgloss.Color(activeTheme.Muted),
		"failed":    lipgloss.Color(activeTheme.Danger),
		"cancelled": lipgloss.Color(activeTheme.Muted),
	}
}

func tuiColorStatus(s string) string {
	if c, ok := tuiStatusColors[s]; ok {
		return lipgloss.NewStyle().Foreground(c).Render(s)
	}
	return s
}

// ── API types ─────────────────────────────────────────────────────────────────

type tuiPipeline struct {
	WorkflowID  string    `json:"workflow_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
}

type tuiRun struct {
	RunID       string     `json:"run_id"`
	WorkflowID  string     `json:"workflow_id"`
	TriggeredBy string     `json:"triggered_by"`
	Status      string     `json:"status"`
	CurrentStep int        `json:"current_step"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at"`
}

type tuiStepRun struct {
	StepRunID     string     `json:"step_run_id"`
	RunID         string     `json:"run_id"`
	StepIndex     int        `json:"step_index"`
	StepName      string     `json:"step_name"`
	ParallelGroup *int       `json:"parallel_group"`
	Status        string     `json:"status"`
	Output        *string    `json:"output"`
	MemoryUsedMB  *int64     `json:"memory_used_mb"`
	MemoryLimitMB *int64     `json:"memory_limit_mb"`
	StartedAt     *time.Time `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at"`
}

type tuiRunFull struct {
	tuiRun
	StepRuns []tuiStepRun `json:"step_runs"`
}

// tuiWorkflowStep is one step of a pipeline definition. ParallelGroup is the only
// place parallelism is recorded: steps sharing the same non-nil group run
// concurrently. The run record carries no grouping, so the run-detail view
// derives it from the definition (see tuiAnnotateParallelGroups).
type tuiWorkflowStep struct {
	StepID        string `json:"step_id"`
	Name          string `json:"name"`
	ParallelGroup *int   `json:"parallel_group"`
}

// tuiPipelineDef is the subset of a pipeline (GET /pipelines/{id}) the run-detail
// view needs: the ordered steps, whose positions line up with step_index.
type tuiPipelineDef struct {
	WorkflowID string            `json:"workflow_id"`
	Steps      []tuiWorkflowStep `json:"steps"`
}

// ── View states ───────────────────────────────────────────────────────────────

type tuiViewID int

const (
	tuiViewPipelines tuiViewID = iota
	tuiViewRuns
	tuiViewRunDetail
	tuiViewOutput
	tuiViewCreate
	tuiViewRun
)

// ── Messages ──────────────────────────────────────────────────────────────────

type tuiPipelinesMsg []tuiPipeline
type tuiRunsMsg []tuiRun
type tuiRunDetailMsg tuiRunFull
type tuiPipelineDefMsg struct {
	workflowID string
	steps      []tuiWorkflowStep
}
type tuiRunDiagramMsg struct {
	runID string
	// workflowID and steps carry the pipeline definition resolved while annotating,
	// so the Update handler can populate m.pipeDefs and later ticks reuse it instead
	// of refetching the (immutable) definition on every refresh.
	workflowID string
	steps      []tuiWorkflowStep
	detail     *tuiRunFull
}
type tuiErrMsg struct{ err error }
type tuiPipelineCreatedMsg struct{}
type tuiRunTriggeredMsg struct{}
type tuiFormErrMsg struct{ err error }

// ── Model ─────────────────────────────────────────────────────────────────────

type tuiModel struct {
	view    tuiViewID
	loading bool
	// submitting guards the create/run forms against double-submit: it is set
	// synchronously the moment a submit dispatches its cmd, and cleared when the
	// resulting success/error message arrives. Without it, two Enter presses
	// landing before the in-flight HTTP POST completes each fire their own cmd.
	submitting bool
	err        error
	width      int
	height     int

	pTable table.Model
	rTable table.Model
	dTable table.Model
	vp     viewport.Model

	pipelines []tuiPipeline
	runs      []tuiRun
	runFull   *tuiRunFull

	// pipeDefs caches each pipeline's ordered step definitions (workflow_id →
	// steps), fetched lazily for the flow diagram shown under the pipelines list.
	// The list endpoint strips steps, so the diagram needs the full GET per
	// pipeline; caching keeps cursor movement from refetching on every keystroke.
	pipeDefs map[string][]tuiWorkflowStep

	// runDetails caches each run's full step detail (run_id → detail), fetched
	// lazily for the live pipeline diagram shown under the runs list. Active runs
	// are refetched on every auto-refresh tick so the diagram animates live.
	runDetails map[string]*tuiRunFull

	selPipeline *tuiPipeline
	selRun      *tuiRun
	outputTitle string

	// editPipelineID is the workflow being edited in tuiViewCreate; "" means the
	// form is a create. It selects PUT vs POST on submit.
	editPipelineID string

	// runStatus is a transient one-line result of a run action (e.g. a cancel
	// failure) shown under the runs list; runStatusErr tints it as an error.
	runStatus    string
	runStatusErr bool

	form      tuiForm
	runReturn tuiViewID // view to restore when the run form is cancelled
}

func tuiTableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color(activeTheme.Border)).
		BorderBottom(true).
		Bold(true).
		Foreground(lipgloss.Color(activeTheme.Text))
	s.Selected = s.Selected.
		Foreground(lipgloss.Color(activeTheme.Accent)).
		Background(lipgloss.Color(activeTheme.SelBg)).
		Bold(false)
	return s
}

// Responsive column specs: flexible columns (description, name, triggered-by,
// step) expand to fill a wider terminal; the rest stay at their minimum.
var (
	ciPipelineCols = []tuiColSpec{
		{"NAME", 16, 2},
		{"DESCRIPTION", 20, 3},
		{"ACTIVE", 6, 0},
		{"CREATED", 14, 0},
	}
	ciRunCols = []tuiColSpec{
		{"RUN ID", 10, 0},
		{"STATUS", 11, 0},
		{"TRIGGERED BY", 16, 1},
		{"STARTED", 18, 0},
		{"DURATION", 10, 0},
	}
	ciStepCols = []tuiColSpec{
		{"#", 3, 0},
		{"STEP", 16, 1},
		{"STATUS", 11, 0},
		{"MEM", 9, 0},
		{"STARTED", 18, 0},
		{"DURATION", 10, 0},
	}
)

func newTUIModel() tuiModel {
	m := tuiModel{
		loading:    true,
		width:      tuiDefaultWidth,
		height:     tuiDefaultHeight,
		pipeDefs:   map[string][]tuiWorkflowStep{},
		runDetails: map[string]*tuiRunFull{},
	}
	m.pTable = table.New(table.WithFocused(true))
	m.pTable.SetStyles(tuiTableStyles())
	m.rTable = table.New(table.WithFocused(true))
	m.rTable.SetStyles(tuiTableStyles())
	m.dTable = table.New(table.WithFocused(true))
	m.dTable.SetStyles(tuiTableStyles())
	m.vp = viewport.New(tuiDefaultWidth-4, tuiDefaultHeight-8)
	m.applyTableLayout()
	return m
}

// tuiDiagReserve is the number of lines the pipeline flow diagram occupies under
// the pipelines list (one caption line plus the joined stage boxes). The
// pipelines table reserves this on top of its normal chrome so the diagram and
// help line always fit; tuiPipelineDiagramPanel renders to exactly this height.
const tuiDiagReserve = 6

// applyTableLayout resizes every table to the current terminal dimensions.
func (m *tuiModel) applyTableLayout() {
	m.pTable.SetColumns(tuiFitColumns(ciPipelineCols, m.width))
	m.pTable.SetHeight(tuiTableHeight(m.height, tuiListChrome+tuiDiagReserve))
	m.rTable.SetColumns(tuiFitColumns(ciRunCols, m.width))
	m.rTable.SetHeight(tuiTableHeight(m.height, tuiListChrome+tuiDiagReserve))
	m.dTable.SetColumns(tuiFitColumns(ciStepCols, m.width))
	m.dTable.SetHeight(tuiTableHeight(m.height, tuiDetailChrome+tuiDiagReserve))
}

// ── Init ──────────────────────────────────────────────────────────────────────

func (m tuiModel) Init() tea.Cmd {
	return tuiFetchPipelines
}

// ── Fetch commands ────────────────────────────────────────────────────────────

func tuiFetchPipelines() tea.Msg {
	data, err := doRequest("GET", appendProjectParam("/workflows/pipelines"), nil)
	if err != nil {
		return tuiErrMsg{err}
	}
	var ps []tuiPipeline
	if err := json.Unmarshal(data, &ps); err != nil {
		return tuiErrMsg{err}
	}
	return tuiPipelinesMsg(ps)
}

func tuiFetchRuns(workflowID string) tea.Cmd {
	return func() tea.Msg {
		path := "/workflows/runs"
		if workflowID != "" {
			path += "?workflow_id=" + url.QueryEscape(workflowID)
		}
		data, err := doRequest("GET", path, nil)
		if err != nil {
			return tuiErrMsg{err}
		}
		var rs []tuiRun
		if err := json.Unmarshal(data, &rs); err != nil {
			return tuiErrMsg{err}
		}
		return tuiRunsMsg(rs)
	}
}

// tuiRunActionMsg is the outcome of a run action (cancel). cancelled identifies
// the run so the runs list can report it; a non-nil err surfaces as a status
// line instead of being silently swallowed.
type tuiRunActionMsg struct {
	cancelled string
	err       error
}

// tuiCancelRun cancels a run via the API. It runs as a cmd (not a blocking call
// inside Update) so the UI never stalls on the request, and it reports failures.
func tuiCancelRun(runID string) tea.Cmd {
	return func() tea.Msg {
		if _, err := doRequest("DELETE", "/workflows/runs/"+runID, nil); err != nil {
			return tuiRunActionMsg{err: err}
		}
		return tuiRunActionMsg{cancelled: runID}
	}
}

// tuiFetchAnnotatedRun fetches a run's full step detail and annotates parallel
// groups, reusing cachedSteps to skip the immutable pipeline-definition refetch.
// Returns the run and the step definition it used (so the caller can cache it).
// Both run fetchers share this; they differ only in the message they wrap it in.
func tuiFetchAnnotatedRun(runID string, cachedSteps []tuiWorkflowStep) (*tuiRunFull, []tuiWorkflowStep, error) {
	data, err := doRequest("GET", "/workflows/runs/"+runID, nil)
	if err != nil {
		return nil, nil, err
	}
	var r tuiRunFull
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, nil, err
	}
	steps := tuiAnnotateParallelGroups(&r, cachedSteps)
	return &r, steps, nil
}

func tuiFetchRunDetail(runID string, cachedSteps []tuiWorkflowStep) tea.Cmd {
	return func() tea.Msg {
		r, _, err := tuiFetchAnnotatedRun(runID, cachedSteps)
		if err != nil {
			return tuiErrMsg{err}
		}
		return tuiRunDetailMsg(*r)
	}
}

// tuiFetchPipelineDef fetches a pipeline's full definition (its ordered steps,
// with parallel groups) for the flow diagram. A fetch/parse failure is folded
// into an empty-steps message rather than a tuiErrMsg: the diagram is auxiliary
// and must never take over the whole pipelines view with an error.
func tuiFetchPipelineDef(workflowID string) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", "/workflows/pipelines/"+workflowID, nil)
		if err != nil {
			return tuiPipelineDefMsg{workflowID: workflowID}
		}
		var def tuiPipelineDef
		if err := json.Unmarshal(data, &def); err != nil {
			return tuiPipelineDefMsg{workflowID: workflowID}
		}
		return tuiPipelineDefMsg{workflowID: workflowID, steps: def.Steps}
	}
}

// tuiEnsureDiagram returns a cmd to fetch the highlighted pipeline's step
// definition for the flow diagram, or nil when there is nothing to fetch or it
// is already cached. Called whenever the pipelines list loads or the cursor
// moves, so the diagram tracks the highlighted row without refetching per key.
func (m tuiModel) tuiEnsureDiagram() tea.Cmd {
	i := m.pTable.Cursor()
	if i < 0 || i >= len(m.pipelines) {
		return nil
	}
	id := m.pipelines[i].WorkflowID
	if _, ok := m.pipeDefs[id]; ok {
		return nil
	}
	return tuiFetchPipelineDef(id)
}

// tuiFetchRunPreview fetches a run's full step detail for the live pipeline
// diagram under the runs list (same data the run-detail view uses, with parallel
// groups annotated). Like the pipeline-def fetch, a failure degrades to an
// empty detail rather than a tuiErrMsg so it never hijacks the runs view.
func tuiFetchRunPreview(runID string, cachedSteps []tuiWorkflowStep) tea.Cmd {
	return func() tea.Msg {
		r, steps, err := tuiFetchAnnotatedRun(runID, cachedSteps)
		if err != nil {
			// The diagram is auxiliary — degrade to an empty detail rather than a
			// tuiErrMsg so a fetch failure never hijacks the runs view.
			return tuiRunDiagramMsg{runID: runID}
		}
		return tuiRunDiagramMsg{runID: runID, workflowID: r.WorkflowID, steps: steps, detail: r}
	}
}

// tuiEnsureRunDiagram returns a cmd to fetch the highlighted run's step detail
// for the live diagram under the runs list, or nil when nothing is needed.
// Uncached runs are always fetched; with refreshActive set, an already-cached
// run that is still running/pending is refetched too so the diagram stays live
// (the auto-refresh tick passes true; plain cursor movement passes false).
func (m tuiModel) tuiEnsureRunDiagram(refreshActive bool) tea.Cmd {
	i := m.rTable.Cursor()
	if i < 0 || i >= len(m.runs) {
		return nil
	}
	r := m.runs[i]
	_, ok := m.runDetails[r.RunID]
	active := r.Status == "running" || r.Status == "pending"
	if ok && !(refreshActive && active) {
		return nil
	}
	// Reuse the cached pipeline definition (immutable) so a live run's per-tick
	// refresh doesn't refetch it; nil falls back to a fetch the first time.
	return tuiFetchRunPreview(r.RunID, m.pipeDefs[r.WorkflowID])
}

// tuiAnnotateParallelGroups best-effort enriches a run from its pipeline
// definition: it fills each step run's ParallelGroup (the run record stores no
// grouping — only the definition does, via steps[].parallel_group, in step_index
// order) and synthesises pending entries for steps the run has not reached yet
// (see tuiFillPendingSteps) so the live diagram shows the whole plan. Any failure
// (no workflow id, def fetch/parse error, or the pipeline was edited since the
// run so step names no longer line up) leaves groups nil and falls back to
// whatever step runs the API returned, ungrouped.
// cachedSteps, when non-nil, supplies the pipeline definition so the function
// skips the GET /pipelines/{id} round-trip — the definition is immutable, so on
// auto-refresh ticks it should be reused from m.pipeDefs rather than refetched.
// Returns the step definition it used (cached or freshly fetched), or nil on
// failure, so the caller can cache it for subsequent ticks.
func tuiAnnotateParallelGroups(r *tuiRunFull, cachedSteps []tuiWorkflowStep) []tuiWorkflowStep {
	if r.WorkflowID == "" {
		return nil
	}
	steps := cachedSteps
	if steps == nil {
		data, err := doRequest("GET", "/workflows/pipelines/"+r.WorkflowID, nil)
		if err != nil {
			return nil
		}
		var def tuiPipelineDef
		if err := json.Unmarshal(data, &def); err != nil {
			return nil
		}
		steps = def.Steps
	}
	for i := range r.StepRuns {
		idx := r.StepRuns[i].StepIndex
		if idx < 0 || idx >= len(steps) {
			continue
		}
		ds := steps[idx]
		// Guard against a definition that drifted since the run: if the step name
		// at this index differs, skip rather than mislabel which steps ran together.
		if ds.Name != "" && ds.Name != r.StepRuns[i].StepName {
			continue
		}
		r.StepRuns[i].ParallelGroup = ds.ParallelGroup
	}
	tuiFillPendingSteps(r, steps)
	return steps
}

// tuiFillPendingSteps appends a synthetic "pending" step run for every step in
// the pipeline definition the run has not started yet, then sorts all step runs
// back into step_index order. The backend only records a step run once a step
// starts, so without this the live diagram (and the run-detail table) would show
// only running and finished steps and silently drop everything still to do.
// Synthetic entries carry no StepRunID, output, or timestamps — they exist purely
// to display the remaining plan, and render as ○ pending like any not-yet-run step.
func tuiFillPendingSteps(r *tuiRunFull, def []tuiWorkflowStep) {
	if len(def) == 0 {
		return
	}
	// Only an in-flight run has steps genuinely still ahead of it. For a run that
	// has already finished (completed/failed/cancelled), steps it never reached will
	// never run, so synthesising "pending" rows for them would misrepresent
	// never-to-run steps as still queued.
	if r.Status != "running" && r.Status != "pending" {
		return
	}
	seen := make(map[int]bool, len(r.StepRuns))
	for _, sr := range r.StepRuns {
		seen[sr.StepIndex] = true
	}
	for i, ds := range def {
		if seen[i] {
			continue
		}
		r.StepRuns = append(r.StepRuns, tuiStepRun{
			RunID:         r.RunID,
			StepIndex:     i,
			StepName:      ds.Name,
			ParallelGroup: ds.ParallelGroup,
			Status:        "pending",
		})
	}
	sort.SliceStable(r.StepRuns, func(a, b int) bool {
		return r.StepRuns[a].StepIndex < r.StepRuns[b].StepIndex
	})
}

// tuiAutoRefresh returns the fetch cmd for the current view so an auto-refresh
// tick updates whatever the user is looking at, silently (no loading flash, so
// the table cursor and scroll position are preserved). Forms and the static
// step-output snapshot are left untouched.
func (m tuiModel) tuiAutoRefresh() tea.Cmd {
	switch m.view {
	case tuiViewPipelines:
		return tuiFetchPipelines
	case tuiViewRuns:
		wid := ""
		if m.selPipeline != nil {
			wid = m.selPipeline.WorkflowID
		}
		// Also refresh the highlighted run's live pipeline diagram so an active
		// run animates step-by-step alongside the run-list refresh.
		return tea.Batch(tuiFetchRuns(wid), m.tuiEnsureRunDiagram(true))
	case tuiViewRunDetail:
		if m.selRun != nil {
			return tuiFetchRunDetail(m.selRun.RunID, m.pipeDefs[m.selRun.WorkflowID])
		}
	}
	return nil
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width - 4
		m.vp.Height = msg.Height - 8
		m.applyTableLayout()
		return m, nil

	case tuiErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case tuiPipelinesMsg:
		m.loading = false
		m.pipelines = []tuiPipeline(msg)
		rows := make([]table.Row, len(m.pipelines))
		for i, p := range m.pipelines {
			active := "●"
			if !p.Active {
				active = "○"
			}
			rows[i] = table.Row{
				p.Name,
				p.Description,
				active,
				p.CreatedAt.Local().Format("Jan 02 15:04"),
			}
		}
		m.pTable.SetRows(rows)
		return m, m.tuiEnsureDiagram()

	case tuiPipelineDefMsg:
		if m.pipeDefs == nil {
			m.pipeDefs = map[string][]tuiWorkflowStep{}
		}
		m.pipeDefs[msg.workflowID] = msg.steps
		return m, nil

	case tuiRunsMsg:
		m.loading = false
		m.runs = []tuiRun(msg)
		rows := make([]table.Row, len(m.runs))
		for i, r := range m.runs {
			rows[i] = table.Row{
				tuiShortID(r.RunID),
				r.Status,
				r.TriggeredBy,
				tuiFormatTime(r.StartedAt),
				tuiFormatDur(r.StartedAt, r.EndedAt),
			}
		}
		m.rTable.SetRows(rows)
		return m, m.tuiEnsureRunDiagram(false)

	case tuiRunDiagramMsg:
		if m.runDetails == nil {
			m.runDetails = map[string]*tuiRunFull{}
		}
		m.runDetails[msg.runID] = msg.detail
		// Cache the pipeline definition resolved during annotation so subsequent
		// ticks reuse it instead of refetching the immutable definition.
		if msg.workflowID != "" && len(msg.steps) > 0 {
			if m.pipeDefs == nil {
				m.pipeDefs = map[string][]tuiWorkflowStep{}
			}
			m.pipeDefs[msg.workflowID] = msg.steps
		}
		return m, nil

	case tuiRunDetailMsg:
		m.loading = false
		rf := tuiRunFull(msg)
		m.runFull = &rf
		m.dTable.SetRows(tuiStepRunRows(rf.StepRuns))
		return m, nil

	case tuiAutoRefreshMsg:
		return m, m.tuiAutoRefresh()

	case tuiPipelineCreatedMsg:
		m.submitting = false
		m.editPipelineID = ""
		m.view = tuiViewPipelines
		m.loading = true
		return m, tuiFetchPipelines

	case pipelineEditLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		var cmd tea.Cmd
		m.form, cmd = newCIPipelineEditForm(msg.name, msg.dsl, msg.desc)
		m.editPipelineID = msg.workflowID
		m.submitting = false
		m.view = tuiViewCreate
		return m, cmd

	case tuiRunTriggeredMsg:
		// Show the freshly-triggered run in the pipeline's run history.
		m.submitting = false
		m.view = tuiViewRuns
		m.loading = true
		wid := ""
		if m.selPipeline != nil {
			wid = m.selPipeline.WorkflowID
		}
		return m, tuiFetchRuns(wid)

	case tuiRunActionMsg:
		if msg.err != nil {
			m.runStatus = "✗ cancel failed: " + msg.err.Error()
			m.runStatusErr = true
			return m, nil
		}
		m.runStatus = "✓ cancelled " + tuiShortID(msg.cancelled)
		m.runStatusErr = false
		// Return to the runs list and refresh so the new status shows.
		m.view = tuiViewRuns
		m.loading = true
		wid := ""
		if m.selPipeline != nil {
			wid = m.selPipeline.WorkflowID
		}
		return m, tuiFetchRuns(wid)

	case tuiFormErrMsg:
		m.submitting = false
		m.form.errMsg = msg.err.Error()
		return m, nil

	case tea.KeyMsg:
		if m.err != nil {
			switch msg.String() {
			case "esc":
				return m, func() tea.Msg { return goHomeMsg{} }
			case "ctrl+c":
				return m, tea.Quit
			case "r":
				m.err = nil
				m.loading = true
				return m, tuiFetchPipelines
			}
			return m, nil
		}
		switch m.view {
		case tuiViewPipelines:
			return m.tuiKeyPipelines(msg)
		case tuiViewRuns:
			return m.tuiKeyRuns(msg)
		case tuiViewRunDetail:
			return m.tuiKeyRunDetail(msg)
		case tuiViewOutput:
			return m.tuiKeyOutput(msg)
		case tuiViewCreate:
			return m.tuiKeyCreate(msg)
		case tuiViewRun:
			return m.tuiKeyRun(msg)
		}
	}

	return m.tuiDelegate(msg)
}

func (m tuiModel) tuiDelegate(msg tea.Msg) (tuiModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.view {
	case tuiViewPipelines:
		m.pTable, cmd = m.pTable.Update(msg)
	case tuiViewRuns:
		m.rTable, cmd = m.rTable.Update(msg)
	case tuiViewRunDetail:
		m.dTable, cmd = m.dTable.Update(msg)
	case tuiViewOutput:
		m.vp, cmd = m.vp.Update(msg)
	case tuiViewCreate, tuiViewRun:
		m.form, _, cmd = m.form.update(msg)
	}
	return m, cmd
}

func (m tuiModel) tuiKeyPipelines(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return goHomeMsg{} }
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		i := m.pTable.Cursor()
		if i < len(m.pipelines) {
			m.selPipeline = &m.pipelines[i]
			m.view = tuiViewRuns
			m.loading = true
			m.runStatus = ""
			return m, tuiFetchRuns(m.selPipeline.WorkflowID)
		}
	case "n":
		m.submitting = false
		m.editPipelineID = ""
		var cmd tea.Cmd
		m.form, cmd = newCIPipelineForm()
		m.view = tuiViewCreate
		return m, cmd
	case "e":
		i := m.pTable.Cursor()
		if i < len(m.pipelines) {
			m.selPipeline = &m.pipelines[i]
			return m, tuiFetchPipelineForEdit(m.selPipeline.WorkflowID)
		}
	case "R":
		i := m.pTable.Cursor()
		if i < len(m.pipelines) {
			m.selPipeline = &m.pipelines[i]
			return m.openRunForm()
		}
	case "r":
		m.loading = true
		// Drop cached step definitions so the flow diagram refetches fresh.
		m.pipeDefs = map[string][]tuiWorkflowStep{}
		return m, tuiFetchPipelines
	}
	// Navigation key: let the table move the cursor, then fetch the newly
	// highlighted pipeline's diagram if it isn't cached yet.
	prev := m.pTable.Cursor()
	var cmd tea.Cmd
	m.pTable, cmd = m.pTable.Update(msg)
	if m.pTable.Cursor() != prev {
		if dcmd := m.tuiEnsureDiagram(); dcmd != nil {
			return m, tea.Batch(cmd, dcmd)
		}
	}
	return m, cmd
}

// ── Create form ───────────────────────────────────────────────────────────────

func newCIPipelineForm() (tuiForm, tea.Cmd) {
	return newTUIForm("New Pipeline",
		formInput("name", "Name", "my-pipeline (required)"),
		formInput("steps", "Steps", "build->test->deploy (required)"),
		formInput("desc", "Desc", "description (optional)"),
	)
}

// newCIPipelineEditForm is the create form pre-filled from an existing pipeline.
func newCIPipelineEditForm(name, dsl, desc string) (tuiForm, tea.Cmd) {
	return newTUIForm("Edit Pipeline",
		formInputDefault("name", "Name", "my-pipeline (required)", name),
		formInputDefault("steps", "Steps", "build->test->deploy (required)", dsl),
		formInputDefault("desc", "Desc", "description (optional)", desc),
	)
}

func (m tuiModel) tuiKeyCreate(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	var (
		action formAction
		cmd    tea.Cmd
	)
	m.form, action, cmd = m.form.update(msg)
	switch action {
	case formCancel:
		m.view = tuiViewPipelines
		m.editPipelineID = ""
		return m, nil
	case formSubmit:
		return m.tuiSubmitCreate()
	}
	return m, cmd
}

// tuiSubmitCreate validates the pipeline form and, if valid, returns the save
// cmd — a create (POST) or, when editing, an update (PUT).
func (m tuiModel) tuiSubmitCreate() (tuiModel, tea.Cmd) {
	if m.submitting {
		return m, nil
	}
	name := m.form.value("name")
	dsl := m.form.value("steps")
	if name == "" {
		m.form.errMsg = "name is required"
		return m, nil
	}
	if dsl == "" {
		m.form.errMsg = "steps are required (e.g. build->test->deploy)"
		return m, nil
	}
	m.form.errMsg = ""
	m.submitting = true
	return m, ciSubmitSavePipeline(m.editPipelineID, name, m.form.value("desc"), dsl)
}

// ciSubmitSavePipeline resolves the DSL step names to IDs and POSTs a new
// pipeline (editID == "") or PUTs an existing one. Name/DSL resolution errors
// surface as an inline form error.
func ciSubmitSavePipeline(editID, name, desc, dsl string) tea.Cmd {
	return func() tea.Msg {
		nodes, err := parseDSL(dsl)
		if err != nil {
			return tuiFormErrMsg{err}
		}
		refs, err := dslToRefs(nodes)
		if err != nil {
			return tuiFormErrMsg{err}
		}
		payload := map[string]any{"name": name, "steps": refs}
		if desc != "" {
			payload["description"] = desc
		}
		method, path := "POST", "/workflows/pipelines"
		if editID != "" {
			// Edit: omit project so the server preserves the stored label
			// (it guards against an empty project wiping it).
			method, path = "PUT", "/workflows/pipelines/"+editID
		} else if p := projectFilter(); p != "" {
			// Create: tag with the current project so the new pipeline isn't
			// hidden by the project-filtered list it was created from.
			payload["project"] = p
		}
		body, _ := json.Marshal(payload)
		if _, err := doRequest(method, path, body); err != nil {
			return tuiFormErrMsg{err}
		}
		return tuiPipelineCreatedMsg{}
	}
}

// pipelineEditLoadedMsg carries a pipeline's current definition, rendered back to
// the DSL, so the edit form can open pre-filled.
type pipelineEditLoadedMsg struct {
	workflowID string
	name       string
	desc       string
	dsl        string
	err        error
}

// tuiFetchPipelineForEdit loads a pipeline's full definition and renders its
// steps back into the DSL so the edit form round-trips faithfully.
func tuiFetchPipelineForEdit(workflowID string) tea.Cmd {
	return func() tea.Msg {
		data, err := doRequest("GET", "/workflows/pipelines/"+workflowID, nil)
		if err != nil {
			return pipelineEditLoadedMsg{err: err}
		}
		var def struct {
			WorkflowID  string            `json:"workflow_id"`
			Name        string            `json:"name"`
			Description string            `json:"description"`
			Steps       []tuiWorkflowStep `json:"steps"`
		}
		if err := json.Unmarshal(data, &def); err != nil {
			return pipelineEditLoadedMsg{err: err}
		}
		return pipelineEditLoadedMsg{workflowID: def.WorkflowID, name: def.Name, desc: def.Description, dsl: stepsToDSL(def.Steps)}
	}
}

// stepsToDSL renders an ordered step list back into the pipeline DSL, collapsing
// consecutive steps that share a parallel group into "[a,b]" segments — the
// inverse of parseDSL.
func stepsToDSL(steps []tuiWorkflowStep) string {
	var segs []string
	for i := 0; i < len(steps); {
		g := steps[i].ParallelGroup
		if g == nil {
			segs = append(segs, steps[i].Name)
			i++
			continue
		}
		var names []string
		for i < len(steps) && steps[i].ParallelGroup != nil && *steps[i].ParallelGroup == *g {
			names = append(names, steps[i].Name)
			i++
		}
		if len(names) == 1 {
			segs = append(segs, names[0])
		} else {
			segs = append(segs, "["+strings.Join(names, ",")+"]")
		}
	}
	return strings.Join(segs, "->")
}

// ── Run form ──────────────────────────────────────────────────────────────────

// openRunForm opens the manual-run dialog for m.selPipeline, remembering the
// current view so a cancel returns the user to where they triggered it from.
func (m tuiModel) openRunForm() (tuiModel, tea.Cmd) {
	m.runReturn = m.view
	m.submitting = false
	var cmd tea.Cmd
	m.form, cmd = newCIRunForm(m.selPipeline.Name)
	m.view = tuiViewRun
	return m, cmd
}

func newCIRunForm(name string) (tuiForm, tea.Cmd) {
	return newTUIForm("Run Pipeline: "+name,
		formInput("inputs", "Inputs", "KEY=VALUE KEY=VALUE (optional)"),
	)
}

func (m tuiModel) tuiKeyRun(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	var (
		action formAction
		cmd    tea.Cmd
	)
	m.form, action, cmd = m.form.update(msg)
	switch action {
	case formCancel:
		m.view = m.runReturn
		return m, nil
	case formSubmit:
		return m.tuiSubmitRun()
	}
	return m, cmd
}

// tuiSubmitRun validates the inputs field and, if valid, returns the trigger cmd.
func (m tuiModel) tuiSubmitRun() (tuiModel, tea.Cmd) {
	if m.submitting {
		return m, nil
	}
	if m.selPipeline == nil {
		m.form.errMsg = "no pipeline selected"
		return m, nil
	}
	inputs, err := parseRunInputs(m.form.value("inputs"))
	if err != nil {
		m.form.errMsg = err.Error()
		return m, nil
	}
	m.form.errMsg = ""
	m.submitting = true
	return m, ciSubmitRunPipeline(m.selPipeline.WorkflowID, inputs)
}

// parseRunInputs parses a whitespace-separated list of KEY=VALUE pairs. An empty
// string yields an empty map, i.e. a run with no inputs. Values may contain '='.
func parseRunInputs(s string) (map[string]string, error) {
	inputs := map[string]string{}
	for tok := range strings.FieldsSeq(s) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid input %q: expected KEY=VALUE", tok)
		}
		inputs[k] = v
	}
	return inputs, nil
}

// ciSubmitRunPipeline triggers a manual run of the pipeline. HTTP failures
// surface as an inline form error.
func ciSubmitRunPipeline(workflowID string, inputs map[string]string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]any{"inputs": inputs})
		if _, err := doRequest("POST", "/workflows/pipelines/"+workflowID+"/runs", body); err != nil {
			return tuiFormErrMsg{err}
		}
		return tuiRunTriggeredMsg{}
	}
}

func (m tuiModel) tuiKeyRuns(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = tuiViewPipelines
		return m, nil
	case "enter":
		i := m.rTable.Cursor()
		if i < len(m.runs) {
			m.selRun = &m.runs[i]
			m.view = tuiViewRunDetail
			m.loading = true
			return m, tuiFetchRunDetail(m.selRun.RunID, m.pipeDefs[m.selRun.WorkflowID])
		}
	case "c":
		i := m.rTable.Cursor()
		if i < len(m.runs) {
			r := m.runs[i]
			if r.Status == "pending" || r.Status == "running" {
				m.runStatus = "cancelling " + tuiShortID(r.RunID) + "…"
				m.runStatusErr = false
				return m, tuiCancelRun(r.RunID)
			}
		}
	case "R":
		if m.selPipeline != nil {
			return m.openRunForm()
		}
	case "r":
		m.loading = true
		m.runStatus = ""
		// Drop cached run details so the live diagram refetches fresh.
		m.runDetails = map[string]*tuiRunFull{}
		wid := ""
		if m.selPipeline != nil {
			wid = m.selPipeline.WorkflowID
		}
		return m, tuiFetchRuns(wid)
	}
	// Navigation key: move the cursor, then fetch the newly highlighted run's
	// live pipeline diagram if it isn't cached yet.
	prev := m.rTable.Cursor()
	var cmd tea.Cmd
	m.rTable, cmd = m.rTable.Update(msg)
	if m.rTable.Cursor() != prev {
		if dcmd := m.tuiEnsureRunDiagram(false); dcmd != nil {
			return m, tea.Batch(cmd, dcmd)
		}
	}
	return m, cmd
}

func (m tuiModel) tuiKeyRunDetail(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = tuiViewRuns
		return m, nil
	case "c":
		// Cancel the run being watched, if it is still in progress.
		if m.selRun != nil && (m.selRun.Status == "pending" || m.selRun.Status == "running") {
			m.runStatus = "cancelling " + tuiShortID(m.selRun.RunID) + "…"
			m.runStatusErr = false
			return m, tuiCancelRun(m.selRun.RunID)
		}
		return m, nil
	case "enter":
		if m.runFull == nil {
			return m, nil
		}
		i := m.dTable.Cursor()
		if i < len(m.runFull.StepRuns) {
			sr := m.runFull.StepRuns[i]
			output := "(no output)"
			if sr.Output != nil && *sr.Output != "" {
				output = *sr.Output
			}
			m.outputTitle = fmt.Sprintf("Step %d: %s  [%s]", sr.StepIndex+1, sr.StepName, sr.Status)
			m.vp.SetContent(output)
			m.vp.GotoTop()
			m.view = tuiViewOutput
		}
	case "r":
		if m.selRun != nil {
			m.loading = true
			return m, tuiFetchRunDetail(m.selRun.RunID, m.pipeDefs[m.selRun.WorkflowID])
		}
	}
	var cmd tea.Cmd
	m.dTable, cmd = m.dTable.Update(msg)
	return m, cmd
}

func (m tuiModel) tuiKeyOutput(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = tuiViewRunDetail
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m tuiModel) View() string {
	var content string
	if m.err != nil {
		msg := "error: " + m.err.Error()
		hint := "[esc] quit"
		if strings.Contains(m.err.Error(), "401") || strings.Contains(m.err.Error(), "unauthorized") {
			hint += "  [r] retry after login\n\n  Not authenticated — run `armory auth login` first"
		}
		content = tuiErrStyle.Render(msg) + "\n\n" + tuiHelpStyle.Render(hint)
	} else {
		switch m.view {
		case tuiViewPipelines:
			content = m.tuiViewPipelines()
		case tuiViewRuns:
			content = m.tuiViewRuns()
		case tuiViewRunDetail:
			content = m.tuiViewRunDetail()
		case tuiViewOutput:
			content = m.tuiViewOutput()
		case tuiViewCreate, tuiViewRun:
			content = m.form.view(m.width, m.height)
		}
	}
	return content
}

func (m tuiModel) tuiViewPipelines() string {
	title := tuiTitleStyle.Render("Pipelines")
	help := tuiHelp("[↑↓/jk] navigate  [enter] runs  [R] run  [n] new  [e] edit  [r] refresh  [esc] home", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if len(m.pipelines) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No pipelines found.") + "\n\n" + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.pTable.View()) + "\n" +
		m.tuiPipelineDiagramPanel() + "\n" + help
}

// tuiPipelineDiagramPanel renders the flow diagram for the highlighted pipeline:
// a caption naming the pipeline plus its step flow. It is clamped to exactly
// tuiDiagReserve lines (the space applyTableLayout sets aside) so the list
// layout stays put as the cursor moves between pipelines of different shapes.
func (m tuiModel) tuiPipelineDiagramPanel() string {
	i := m.pTable.Cursor()
	if i < 0 || i >= len(m.pipelines) {
		return tuiClampHeight("", tuiDiagReserve)
	}
	p := m.pipelines[i]
	caption := tuiMetaStyle.Render("▾ " + tuiTrunc(p.Name, 48) + " — pipeline flow")

	var body string
	steps, ok := m.pipeDefs[p.WorkflowID]
	switch {
	case !ok:
		body = tuiMetaStyle.Render("loading…")
	case len(steps) == 0:
		body = tuiMetaStyle.Render("(no steps defined)")
	default:
		body = tuiPipelineDiagram(steps, m.width)
	}
	return tuiClampHeight(caption+"\n"+body, tuiDiagReserve)
}

func (m tuiModel) tuiViewRuns() string {
	name := ""
	if m.selPipeline != nil {
		name = ": " + m.selPipeline.Name
	}
	title := tuiTitleStyle.Render("Runs" + name)
	help := tuiHelp("[↑↓/jk] navigate  [enter] detail  [R] run  [c] cancel  [r] refresh  [esc] back", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	status := ""
	if m.runStatus != "" {
		if m.runStatusErr {
			status = tuiErrStyle.Render(m.runStatus) + "\n"
		} else {
			status = tuiMetaStyle.Render(m.runStatus) + "\n"
		}
	}
	if len(m.runs) == 0 {
		return title + "\n\n" + tuiMetaStyle.Render("No runs found.") + "\n\n" + status + help
	}
	return title + "\n" + tuiBoxStyle.Render(m.rTable.View()) + "\n" +
		m.tuiRunDiagramPanel() + "\n" + status + help
}

// tuiRunDiagramPanel renders the live pipeline diagram for the highlighted run
// under the runs list: each step box coloured by its live status, so an active
// run lights up stage by stage as the panel auto-refreshes. Clamped to exactly
// tuiDiagReserve lines (reserved by applyTableLayout) so the layout stays put.
func (m tuiModel) tuiRunDiagramPanel() string {
	i := m.rTable.Cursor()
	if i < 0 || i >= len(m.runs) {
		return tuiClampHeight("", tuiDiagReserve)
	}
	r := m.runs[i]
	caption := tuiMetaStyle.Render("▾ live pipeline · "+tuiShortID(r.RunID)+" · ") + tuiColorStatus(r.Status)

	var body string
	detail, ok := m.runDetails[r.RunID]
	switch {
	case !ok:
		body = tuiMetaStyle.Render("loading…")
	case detail == nil || len(detail.StepRuns) == 0:
		body = tuiMetaStyle.Render("(no steps)")
	default:
		body = tuiRunDiagram(detail.StepRuns, m.width)
	}
	return tuiClampHeight(caption+"\n"+body, tuiDiagReserve)
}

func (m tuiModel) tuiViewRunDetail() string {
	title := tuiTitleStyle.Render("Run Detail")
	help := tuiHelp("[↑↓/jk] navigate  [enter] output  [r] refresh  [esc] back", m.width)
	if m.loading {
		return title + "\n\n" + tuiMetaStyle.Render("Loading…") + "\n\n" + help
	}
	if m.runFull == nil {
		return title + "\n\n" + tuiMetaStyle.Render("No data.") + "\n\n" + help
	}
	d := m.runFull
	live := ""
	if d.Status == "running" || d.Status == "pending" {
		live = "  " + tuiMetaStyle.Render("(auto-refreshing)")
	}
	meta := fmt.Sprintf("run %s  status: %s  triggered: %s",
		tuiShortID(d.RunID),
		tuiColorStatus(d.Status),
		tuiTrunc(d.TriggeredBy, 20),
	)
	if d.StartedAt != nil {
		meta += "  started: " + d.StartedAt.Local().Format("Jan 02 15:04:05")
	}
	if tuiRunHasParallel(d.StepRuns) {
		meta += "\n┌├└ bracketed steps ran in parallel"
	}
	diagram := tuiClampHeight(
		tuiMetaStyle.Render("▾ live pipeline")+"\n"+tuiRunDiagram(d.StepRuns, m.width),
		tuiDiagReserve)
	return title + live + "\n" + tuiMetaStyle.Render(meta) + "\n" +
		diagram + "\n" + tuiBoxStyle.Render(m.dTable.View()) + "\n" + help
}

func (m tuiModel) tuiViewOutput() string {
	title := tuiTitleStyle.Render(m.outputTitle)
	help := tuiHelp("[↑↓/pgup/pgdn] scroll  [esc] back", m.width)
	return title + "\n" + tuiBoxStyle.Render(m.vp.View()) + "\n" + help
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// tuiStepRunRows builds the run-detail table rows, collapsing each parallel
// batch — consecutive step runs sharing the same non-nil ParallelGroup — into a
// bracketed block that shares one stage number, so it is visually obvious those
// steps ran concurrently. Sequential steps render as a normal single row. Rows
// stay 1:1 with stepRuns in order, so the table cursor still indexes StepRuns.
func tuiStepRunRows(stepRuns []tuiStepRun) []table.Row {
	rows := make([]table.Row, 0, len(stepRuns))
	stage := 0
	i := 0
	for i < len(stepRuns) {
		stage++
		// Extent of this batch: consecutive runs sharing the same non-nil group.
		j := i + 1
		if g := stepRuns[i].ParallelGroup; g != nil {
			for j < len(stepRuns) && stepRuns[j].ParallelGroup != nil && *stepRuns[j].ParallelGroup == *g {
				j++
			}
		}
		batch := stepRuns[i:j]
		if len(batch) == 1 {
			sr := batch[0]
			rows = append(rows, table.Row{
				fmt.Sprintf("%d", stage),
				sr.StepName,
				sr.Status,
				tuiFormatMemShort(sr.MemoryUsedMB, sr.MemoryLimitMB),
				tuiFormatTime(sr.StartedAt),
				tuiFormatDur(sr.StartedAt, sr.EndedAt),
			})
			i = j
			continue
		}
		for k, sr := range batch {
			// ┌/├/└ bracket the members into one group; the stage number sits on
			// the first row only so the block reads as a single parallel stage.
			glyph, num := "├ ", ""
			switch {
			case k == 0:
				glyph, num = "┌ ", fmt.Sprintf("%d", stage)
			case k == len(batch)-1:
				glyph = "└ "
			}
			rows = append(rows, table.Row{
				num,
				glyph + sr.StepName,
				sr.Status,
				tuiFormatMemShort(sr.MemoryUsedMB, sr.MemoryLimitMB),
				tuiFormatTime(sr.StartedAt),
				tuiFormatDur(sr.StartedAt, sr.EndedAt),
			})
		}
		i = j
	}
	return rows
}

// tuiPipelineStages groups a pipeline's ordered steps into stages for the flow
// diagram: consecutive steps sharing the same non-nil parallel group collapse
// into one stage (they run concurrently); every other step is its own stage.
// This mirrors the run-detail bracketing in tuiStepRunRows.
func tuiPipelineStages(steps []tuiWorkflowStep) [][]string {
	var stages [][]string
	for i := 0; i < len(steps); {
		j := i + 1
		if g := steps[i].ParallelGroup; g != nil {
			for j < len(steps) && steps[j].ParallelGroup != nil && *steps[j].ParallelGroup == *g {
				j++
			}
		}
		names := make([]string, 0, j-i)
		for _, s := range steps[i:j] {
			names = append(names, s.Name)
		}
		stages = append(stages, names)
		i = j
	}
	return stages
}

// tuiPipelineDiagram renders the pipeline's steps as a left-to-right flow of
// bordered stage boxes joined by arrows, parallel stages stacked in one
// accent-bordered box. When the boxed form would overflow the terminal width it
// falls back to a compact one-line "a → [b, c] → d" rendering wrapped to width.
func tuiPipelineDiagram(steps []tuiWorkflowStep, width int) string {
	stages := tuiPipelineStages(steps)
	if len(stages) == 0 {
		return tuiMetaStyle.Render("(no steps defined)")
	}

	parts := make([]string, 0, len(stages)*2-1)
	for i, st := range stages {
		if i > 0 {
			parts = append(parts, tuiDiagArrowStyle.Render(" → "))
		}
		parts = append(parts, tuiStageBox(st))
	}
	diagram := lipgloss.JoinHorizontal(lipgloss.Center, parts...)
	if width <= 0 || lipgloss.Width(diagram) <= width {
		return diagram
	}
	return tuiCompactFlow(stages, width)
}

// tuiStageBox renders one stage as a bordered box. A single (sequential) step
// uses the plain border; a parallel stage stacks its step names and uses the
// accent border so it reads as "these run together". Names are truncated and a
// stage with more than three steps is summarised with a "+N more" line so the
// box never grows past the reserved diagram height.
func tuiStageBox(names []string) string {
	const (
		maxRows = 3
		maxName = 16
	)
	lines := make([]string, 0, maxRows)
	if len(names) > maxRows {
		for _, n := range names[:maxRows-1] {
			lines = append(lines, tuiTrunc(n, maxName))
		}
		lines = append(lines, fmt.Sprintf("+%d more", len(names)-(maxRows-1)))
	} else {
		for _, n := range names {
			lines = append(lines, tuiTrunc(n, maxName))
		}
	}
	style := tuiDiagStepStyle
	if len(names) > 1 {
		style = tuiDiagParStyle
	}
	return style.Render(strings.Join(lines, "\n"))
}

// tuiCompactFlow is the width-constrained fallback for the flow diagram: stages
// on one line, parallel stages bracketed, joined by arrows and wrapped to width.
func tuiCompactFlow(stages [][]string, width int) string {
	parts := make([]string, len(stages))
	for i, st := range stages {
		if len(st) == 1 {
			parts[i] = st[0]
		} else {
			parts[i] = "[" + strings.Join(st, ", ") + "]"
		}
	}
	s := strings.Join(parts, " → ")
	if width > 0 {
		return lipgloss.NewStyle().Width(width).Render(s)
	}
	return s
}

// tuiClampHeight pads or truncates s to exactly n lines, so a variable-height
// block occupies a fixed slot in a layout.
func tuiClampHeight(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// ── Live run diagram ──────────────────────────────────────────────────────────

// tuiStatusGlyph is a compact per-step status marker so the live diagram reads
// even without colour (✓ done, ● running, ○ pending, ✗ failed, ⊘ cancelled).
func tuiStatusGlyph(status string) string {
	switch status {
	case "completed":
		return "✓"
	case "running":
		return "●"
	case "failed":
		return "✗"
	case "cancelled":
		return "⊘"
	default: // pending / unknown
		return "○"
	}
}

// tuiStatusColor maps a status to its themed colour, matching the portal's
// pill tones (green=done, amber=running, red=failed, dim=pending/cancelled).
func tuiStatusColor(status string) lipgloss.Color {
	if c, ok := tuiStatusColors[status]; ok {
		return c
	}
	return lipgloss.Color(activeTheme.Muted)
}

// tuiRunBatches groups ordered step runs into stages for the diagram: runs of
// consecutive steps sharing the same non-nil parallel group form one stage.
func tuiRunBatches(stepRuns []tuiStepRun) [][]tuiStepRun {
	var out [][]tuiStepRun
	for i := 0; i < len(stepRuns); {
		j := i + 1
		if g := stepRuns[i].ParallelGroup; g != nil {
			for j < len(stepRuns) && stepRuns[j].ParallelGroup != nil && *stepRuns[j].ParallelGroup == *g {
				j++
			}
		}
		out = append(out, stepRuns[i:j])
		i = j
	}
	return out
}

// tuiStageStatus reduces a stage's step statuses to one, by precedence
// failed > running > pending > cancelled > completed — the colour the stage box
// border takes (a failed step makes the whole stage read as failed, etc.).
func tuiStageStatus(batch []tuiStepRun) string {
	rank := map[string]int{"completed": 0, "cancelled": 1, "pending": 2, "running": 3, "failed": 4}
	best, bestRank := "completed", 0
	for _, sr := range batch {
		if r, ok := rank[sr.Status]; ok && r >= bestRank {
			best, bestRank = sr.Status, r
		}
	}
	return best
}

// tuiRunDiagram renders the run as a left-to-right flow of stage boxes coloured
// by live status, joined by arrows; parallel steps stack in one box. It falls
// back to a compact coloured one-liner when the boxed form overflows the width.
func tuiRunDiagram(stepRuns []tuiStepRun, width int) string {
	if len(stepRuns) == 0 {
		return tuiMetaStyle.Render("(no steps)")
	}
	batches := tuiRunBatches(stepRuns)
	parts := make([]string, 0, len(batches)*2-1)
	for i, b := range batches {
		if i > 0 {
			parts = append(parts, tuiDiagArrowStyle.Render(" → "))
		}
		parts = append(parts, tuiRunStageBox(b))
	}
	diagram := lipgloss.JoinHorizontal(lipgloss.Center, parts...)
	if width <= 0 || lipgloss.Width(diagram) <= width {
		return diagram
	}
	return tuiRunCompactFlow(batches, width)
}

// tuiRunStageBox renders one live stage: each step "glyph name" coloured by its
// status, stacked, in a box whose border takes the stage's aggregate status
// colour. Long stages are summarised with "+N more" to stay within the reserved
// diagram height.
func tuiRunStageBox(batch []tuiStepRun) string {
	const (
		maxRows = 3
		maxName = 16
	)
	line := func(sr tuiStepRun) string {
		txt := tuiStatusGlyph(sr.Status) + " " + tuiTrunc(sr.StepName, maxName)
		return lipgloss.NewStyle().Foreground(tuiStatusColor(sr.Status)).Render(txt)
	}
	lines := make([]string, 0, maxRows)
	if len(batch) > maxRows {
		for _, sr := range batch[:maxRows-1] {
			lines = append(lines, line(sr))
		}
		lines = append(lines, tuiMetaStyle.Render(fmt.Sprintf("+%d more", len(batch)-(maxRows-1))))
	} else {
		for _, sr := range batch {
			lines = append(lines, line(sr))
		}
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tuiStatusColor(tuiStageStatus(batch))).
		Padding(0, 1)
	return box.Render(strings.Join(lines, "\n"))
}

// tuiRunCompactFlow is the width-constrained fallback for the live diagram:
// status-coloured "glyph name" labels, parallel stages bracketed, joined by
// arrows and wrapped to width.
func tuiRunCompactFlow(batches [][]tuiStepRun, width int) string {
	stageStrs := make([]string, len(batches))
	for i, b := range batches {
		labels := make([]string, len(b))
		for k, sr := range b {
			txt := tuiStatusGlyph(sr.Status) + " " + sr.StepName
			labels[k] = lipgloss.NewStyle().Foreground(tuiStatusColor(sr.Status)).Render(txt)
		}
		s := strings.Join(labels, ", ")
		if len(b) > 1 {
			s = "[" + s + "]"
		}
		stageStrs[i] = s
	}
	joined := strings.Join(stageStrs, tuiDiagArrowStyle.Render(" → "))
	if width > 0 {
		return lipgloss.NewStyle().Width(width).Render(joined)
	}
	return joined
}

// tuiRunHasParallel reports whether any two consecutive step runs share a non-nil
// parallel group, i.e. the run contains a parallel batch worth a legend.
func tuiRunHasParallel(stepRuns []tuiStepRun) bool {
	for i := 0; i+1 < len(stepRuns); i++ {
		a, b := stepRuns[i].ParallelGroup, stepRuns[i+1].ParallelGroup
		if a != nil && b != nil && *a == *b {
			return true
		}
	}
	return false
}

func tuiShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func tuiTrunc(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func tuiFormatTime(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Local().Format("Jan 02 15:04:05")
}

func tuiFormatDur(start, end *time.Time) string {
	if start == nil {
		return "—"
	}
	e := time.Now()
	if end != nil {
		e = *end
	}
	d := e.Sub(*start).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

// formatMem picks one of four format strings based on which of used/limit are
// known, so the field-presence logic lives in one place and the long and short
// variants can't drift. both takes two ints, usedOnly/limitOnly take one, and
// none is returned verbatim when neither is known.
func formatMem(usedMB, limitMB *int64, both, usedOnly, limitOnly, none string) string {
	switch {
	case usedMB != nil && limitMB != nil:
		return fmt.Sprintf(both, *usedMB, *limitMB)
	case usedMB != nil:
		return fmt.Sprintf(usedOnly, *usedMB)
	case limitMB != nil:
		return fmt.Sprintf(limitOnly, *limitMB)
	default:
		return none
	}
}

// tuiFormatMem renders a run's memory as "used / limit MB" when both are known,
// falling back to the limit alone (suffixed "limit") when usage was not captured
// — a short job a metrics-server never sampled. Returns "" when neither is known
// so callers can omit the field. tuiFormatMemShort is the compact table variant.
func tuiFormatMem(usedMB, limitMB *int64) string {
	return formatMem(usedMB, limitMB, "%d / %d MB", "%d MB", "%d MB limit", "")
}

// tuiFormatMemShort is the table-cell form: "180/256", "256↑" for limit-only, or
// "—" when unknown. Kept narrow so it fits a fixed-width column.
func tuiFormatMemShort(usedMB, limitMB *int64) string {
	return formatMem(usedMB, limitMB, "%d/%d", "%d", "%d↑", "—")
}

// ── Command ───────────────────────────────────────────────────────────────────

var ciTUICmd = &cobra.Command{
	Use:   "tui",
	Short: "Interactive TUI for browsing pipelines and runs",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, args []string) error { return startCITUI() },
}

func startCITUI() error {
	p := tea.NewProgram(standaloneWrap{newTUIModel()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func init() {
	RegisterModule(Module{
		Name:    "workflows",
		Order:   10,
		Command: ciCmd,
		Screens: []HubScreen{
			{Title: "Pipelines", Desc: "Browse workflow pipelines and run history", New: func() tea.Model { return newTUIModel() }},
			{Title: "Steps", Desc: "Browse and create reusable pipeline steps", New: func() tea.Model { return newStepsModel() }},
		},
	})
}
