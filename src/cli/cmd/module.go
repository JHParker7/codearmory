package cmd

import (
	"sort"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// HubScreen is an interactive screen a module contributes to the home menu of
// the `armory` TUI. New builds a fresh model each time the screen is opened.
type HubScreen struct {
	Title string
	Desc  string
	New   func() tea.Model
}

// Module is a self-contained slice of the CLI: a cobra command tree plus any
// interactive screens it adds to the home hub. A service registers one from its
// init(); both the root command wiring (Execute) and the TUI hub (newAppModel)
// are built from the registry, so adding — or swapping — a service touches only
// its own file rather than root.go and home_tui.go.
//
// Slot marks a module as one interchangeable provider of a capability. Modules
// that share a Slot (e.g. a Gitea-backed and a GitHub-backed "repos") are
// alternatives: exactly one is active, chosen by the "providers" map in the CLI
// config and defaulting to the first registered. A module with an empty Slot is
// always active. Order sets the home-menu position (ascending).
type Module struct {
	Name    string
	Slot    string
	Order   int
	Command *cobra.Command
	Screens []HubScreen
}

var moduleRegistry []Module

// RegisterModule adds m to the registry. Call it from a package init().
func RegisterModule(m Module) { moduleRegistry = append(moduleRegistry, m) }

// activeModules resolves the registry into the set of modules to wire in,
// collapsing each capability Slot to a single provider and ordering the result
// by Order (stable within equal Order). Safe to call repeatedly.
func activeModules() []Module {
	providers := loadConfig().Providers

	// Gather provider options per slot, preserving registration order.
	bySlot := map[string][]Module{}
	for _, m := range moduleRegistry {
		if m.Slot != "" {
			bySlot[m.Slot] = append(bySlot[m.Slot], m)
		}
	}

	var out []Module
	doneSlot := map[string]bool{}
	for _, m := range moduleRegistry {
		if m.Slot == "" {
			out = append(out, m)
			continue
		}
		if doneSlot[m.Slot] {
			continue
		}
		doneSlot[m.Slot] = true
		out = append(out, pickProvider(bySlot[m.Slot], providers[m.Slot]))
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// pickProvider chooses the module named want from opts, falling back to the
// first registered provider when want is empty or unknown. opts is never empty.
func pickProvider(opts []Module, want string) Module {
	if want != "" {
		for _, m := range opts {
			if m.Name == want {
				return m
			}
		}
	}
	return opts[0]
}

var wireOnce sync.Once

// wireModules attaches every active module's command to the root command. It is
// idempotent (guarded by wireOnce) and called from Execute() in production and
// from TestMain in tests, after all init() registrations have run — the registry
// cannot be wired from an init() because Go's per-file init order would let some
// modules register after root.go.
func wireModules() {
	wireOnce.Do(func() {
		for _, m := range activeModules() {
			if m.Command != nil {
				rootCmd.AddCommand(m.Command)
			}
		}
	})
}

// hubScreens returns the home-menu screens of all active modules, in module
// Order then per-module Screens order.
func hubScreens() []HubScreen {
	var out []HubScreen
	for _, m := range activeModules() {
		out = append(out, m.Screens...)
	}
	return out
}
