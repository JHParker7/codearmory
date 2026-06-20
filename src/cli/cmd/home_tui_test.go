package cmd

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// recordModel is a stub screen used to verify the host forwards auto-refresh
// ticks. It flags receipt of a tuiAutoRefreshMsg.
type recordModel struct{ got *bool }

func (m recordModel) Init() tea.Cmd { return nil }
func (m recordModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tuiAutoRefreshMsg); ok {
		*m.got = true
	}
	return m, nil
}
func (m recordModel) View() string { return "" }

// ── Auto-refresh host wiring ──────────────────────────────────────────────────

func TestAppModel_InitStartsTicker(t *testing.T) {
	if newAppModel().Init() == nil {
		t.Error("appModel.Init should start the auto-refresh ticker")
	}
}

func TestAppModel_AutoRefresh_ForwardsToActiveAndReschedules(t *testing.T) {
	got := false
	m := appModel{active: recordModel{got: &got}}
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if !got {
		t.Error("auto-refresh should be forwarded to the active screen")
	}
	if cmd == nil {
		t.Error("auto-refresh should reschedule the ticker")
	}
}

func TestAppModel_AutoRefresh_OnHomeReschedulesOnly(t *testing.T) {
	m := appModel{} // active == nil → home menu
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd == nil {
		t.Error("auto-refresh on the home menu should still reschedule the ticker")
	}
}

func TestStandaloneWrap_InitStartsTicker(t *testing.T) {
	w := standaloneWrap{inner: recordModel{got: new(bool)}}
	if w.Init() == nil {
		t.Error("standaloneWrap.Init should start the auto-refresh ticker")
	}
}

func TestStandaloneWrap_AutoRefresh_ForwardsAndReschedules(t *testing.T) {
	got := false
	w := standaloneWrap{inner: recordModel{got: &got}}
	_, cmd := w.Update(tuiAutoRefreshMsg{})
	if !got {
		t.Error("auto-refresh should be forwarded to the inner model")
	}
	if cmd == nil {
		t.Error("auto-refresh should reschedule the ticker")
	}
}

// Home rows must never wrap: a description longer than its column would
// otherwise spill its second line under the name column (the bug that the long
// Gatekeeper description triggered).
func TestHomeRows_StaySingleLine(t *testing.T) {
	entries := []homeEntry{
		{name: "Short", desc: "x"},
		{name: "Gatekeeper", desc: "Teams, invites, service requests, roles, orgs, users"},
		{name: strings.Repeat("x", 40), desc: strings.Repeat("y", 90)},
	}
	for _, cursor := range []int{0, 1, 2} {
		for i, row := range homeRows(entries, cursor) {
			if h := lipgloss.Height(row); h != 1 {
				t.Errorf("cursor=%d row %d wrapped to %d lines: %q", cursor, i, h, row)
			}
		}
	}
}

// The registered Gatekeeper screen's description fits the column without being
// truncated, so it reads in full on the admin menu. Gatekeeper is an admin
// screen, so it lives in adminScreens(), not the user hub.
func TestHomeGatekeeperDescFits(t *testing.T) {
	for _, s := range adminScreens() {
		if s.Title == "Gatekeeper" {
			if len(s.Desc) > homeDescWidth {
				t.Errorf("Gatekeeper desc %q is %d chars, exceeds column width %d", s.Desc, len(s.Desc), homeDescWidth)
			}
			return
		}
	}
	t.Fatal("Gatekeeper screen not registered in the admin hub")
}
