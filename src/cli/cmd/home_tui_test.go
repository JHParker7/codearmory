package cmd

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

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
// truncated, so it reads in full on the home menu.
func TestHomeGatekeeperDescFits(t *testing.T) {
	for _, s := range hubScreens() {
		if s.Title == "Gatekeeper" {
			if len(s.Desc) > homeDescWidth {
				t.Errorf("Gatekeeper desc %q is %d chars, exceeds column width %d", s.Desc, len(s.Desc), homeDescWidth)
			}
			return
		}
	}
	t.Fatal("Gatekeeper screen not registered")
}
