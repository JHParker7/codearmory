package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// tuiFetchRunInputs GETs the pipeline and surfaces its declared inputs so the run
// form can prompt for them.
func TestTuiFetchRunInputs_ReturnsDeclaredInputs(t *testing.T) {
	body := `{"workflow_id":"wf-1","name":"deploy","inputs":[{"name":"ENV","default":"staging","required":true}]}`
	srv, rec := recordingServer(t, http.StatusOK, body)
	setupCLI(t, srv)

	msg := tuiFetchRunInputs("wf-1")()
	if rec.Method != "GET" || rec.Path != "/workflows/pipelines/wf-1" {
		t.Errorf("request = %s %s, want GET /workflows/pipelines/wf-1", rec.Method, rec.Path)
	}
	loaded, ok := msg.(runInputsLoadedMsg)
	if !ok {
		t.Fatalf("msg type = %T, want runInputsLoadedMsg", msg)
	}
	if len(loaded.inputs) != 1 || loaded.inputs[0].Name != "ENV" || !loaded.inputs[0].Required {
		t.Errorf("inputs = %#v", loaded.inputs)
	}
}

// A failed fetch degrades to no declared inputs (the free-text field stands) rather
// than an error that would hijack the run dialog.
func TestTuiFetchRunInputs_HTTPError_DegradesToNoInputs(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError, `boom`)
	setupCLI(t, srv)

	msg := tuiFetchRunInputs("wf-1")()
	loaded, ok := msg.(runInputsLoadedMsg)
	if !ok {
		t.Fatalf("msg type = %T, want runInputsLoadedMsg", msg)
	}
	if len(loaded.inputs) != 0 {
		t.Errorf("a failed fetch should degrade to no declared inputs, got %#v", loaded.inputs)
	}
}

// When declared inputs arrive while the run form is open, the form is rebuilt with a
// prompt per input, each pre-filled with its default.
func TestTUIModel_RunInputsLoaded_RebuildsFormWithPrompts(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRun
	m.selPipeline = &tuiPipeline{WorkflowID: "wf-1", Name: "deploy"}
	m.form, _ = newCIRunForm("deploy")

	updated, _ := m.Update(runInputsLoadedMsg{
		workflowID: "wf-1",
		name:       "deploy",
		inputs:     []pipelineInputDef{{Name: "ENV", Default: "staging", Required: true}},
	})
	m2 := updated.(tuiModel)
	if len(m2.runInputDefs) != 1 {
		t.Fatalf("runInputDefs = %#v, want 1 declared input", m2.runInputDefs)
	}
	if m2.form.value("ENV") != "staging" {
		t.Errorf("declared-input field ENV = %q, want the pre-filled default 'staging'", m2.form.value("ENV"))
	}
}

// A stale declared-inputs response for a pipeline the user has navigated away from
// is ignored.
func TestTUIModel_RunInputsLoaded_IgnoredWhenNavigatedAway(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRuns // no longer on the run form
	m.selPipeline = &tuiPipeline{WorkflowID: "wf-1", Name: "deploy"}

	updated, _ := m.Update(runInputsLoadedMsg{
		workflowID: "wf-1",
		inputs:     []pipelineInputDef{{Name: "ENV"}},
	})
	if len(updated.(tuiModel).runInputDefs) != 0 {
		t.Error("a stale runInputsLoadedMsg off the run form should be ignored")
	}
}

// Submitting with a blank required declared input is rejected inline before the
// trigger fires.
func TestTUIModel_SubmitRun_DeclaredInputs_MissingRequired_Errors(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRun
	m.selPipeline = &tuiPipeline{WorkflowID: "wf-1", Name: "deploy"}
	m.runInputDefs = []pipelineInputDef{{Name: "ENV", Required: true}}
	m.form, _ = newCIRunFormWithInputs("deploy", m.runInputDefs) // no default → empty

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m2 := updated.(tuiModel)
	if m2.form.errMsg == "" {
		t.Error("submitting with a blank required declared input should set an inline error")
	}
	if cmd != nil {
		t.Error("an invalid declared-input submit should not emit a trigger cmd")
	}
	if m2.submitting {
		t.Error("an invalid submit should not set the submitting guard")
	}
}

// A valid declared-input submit posts the collected values (including a pre-filled
// default) as the run inputs.
func TestTUIModel_SubmitRun_DeclaredInputs_PostsValues(t *testing.T) {
	mux := http.NewServeMux()
	var postBody []byte
	mux.HandleFunc("/workflows/pipelines/wf-1/runs", func(w http.ResponseWriter, r *http.Request) {
		postBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"run_id":"run-1"}`)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	m := newTUIModel()
	m.view = tuiViewRun
	m.selPipeline = &tuiPipeline{WorkflowID: "wf-1", Name: "deploy"}
	m.runInputDefs = []pipelineInputDef{{Name: "ENV", Default: "staging", Required: true}}
	m.form, _ = newCIRunFormWithInputs("deploy", m.runInputDefs)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m2 := updated.(tuiModel)
	if cmd == nil {
		t.Fatal("a valid declared-input submit should emit a trigger cmd")
	}
	if !m2.submitting {
		t.Error("a valid submit should set the submitting guard")
	}
	cmd() // fire the HTTP trigger
	var got map[string]any
	if err := json.Unmarshal(postBody, &got); err != nil {
		t.Fatalf("run body not JSON: %v", err)
	}
	inputs, _ := got["inputs"].(map[string]any)
	if inputs["ENV"] != "staging" {
		t.Errorf("inputs = %v, want ENV=staging (the pre-filled default)", got["inputs"])
	}
}
