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
//
// Admin marks a module as platform administration (e.g. gatekeeper, forge
// runtimes, audit, org/role management) that a regular user does not touch. Its
// Command is wired under `armory admin` instead of the root, and its Screens go
// into the separate admin hub (adminScreens) rather than the user hub
// (hubScreens). Permission enforcement stays server-side; the split is purely
// organisational, keeping the top-level surface focused on developer tasks.
//
// Service names the builder catalog service this module is a front-end for (e.g.
// "forge", "gitea_integration"). When that service is disabled for the caller's
// org, the live TUI hub drops the module entirely (see enabledScreensFor), so a
// deployment with most services off shows a short, relevant menu rather than a
// wall of dead entries. Leave it empty for account/core modules (settings, auth,
// org-services) that must always be reachable — those are never hidden. The
// filter is hub-only; cobra commands stay wired so scripts keep working (a
// disabled service simply 403s).
type Module struct {
	Name    string
	Slot    string
	Service string
	Admin   bool
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

// wireModules attaches every active module's command to the command tree. It is
// idempotent (guarded by wireOnce) and called from Execute() in production and
// from TestMain in tests, after all init() registrations have run — the registry
// cannot be wired from an init() because Go's per-file init order would let some
// modules register after root.go. User module commands hang off the root;
// admin module commands hang off `armory admin`.
func wireModules() {
	wireOnce.Do(func() {
		rootCmd.AddCommand(adminCmd)
		for _, m := range activeModules() {
			if m.Command == nil {
				continue
			}
			if m.Admin {
				adminCmd.AddCommand(m.Command)
			} else {
				rootCmd.AddCommand(m.Command)
			}
		}
	})
}

// hubScreens returns the user hub's screens — the home-menu screens of all
// active non-admin modules, in module Order then per-module Screens order.
func hubScreens() []HubScreen { return screensFor(false) }

// adminScreens returns the admin hub's screens — those of active admin modules,
// shown under `armory admin` rather than the user home menu.
func adminScreens() []HubScreen { return screensFor(true) }

// screensFor collects the screens of active modules whose Admin flag matches.
func screensFor(admin bool) []HubScreen {
	var out []HubScreen
	for _, m := range activeModules() {
		if m.Admin == admin {
			out = append(out, m.Screens...)
		}
	}
	return out
}

// enabledScreensFor is screensFor restricted to services that are actually up: a
// module whose Service is not registered/routable in conductor (per
// registeredServices) is dropped from the hub entirely. The live TUI entry points
// use this so services that aren't deployed vanish from the menu; screensFor stays
// unfiltered for tests and any non-TUI caller. Modules with no Service
// (account/core) are always kept, and registeredServices fails open, so this never
// hides more than it should.
func enabledScreensFor(admin bool) []HubScreen {
	return filterScreens(admin, registeredServices())
}

// filterScreens is the pure core of enabledScreensFor: it collects the screens of
// active modules whose Admin flag matches, skipping any module whose Service is
// NOT in registered. A nil registered map means "unknown" (fail open) — nothing is
// hidden; a non-nil map hides any service-backed module outside it. Split out from
// the fetch so the hiding logic is testable without a live conductor.
func filterScreens(admin bool, registered map[string]bool) []HubScreen {
	var out []HubScreen
	for _, m := range activeModules() {
		if m.Admin != admin {
			continue
		}
		if m.Service != "" && registered != nil && !registered[m.Service] {
			continue
		}
		out = append(out, m.Screens...)
	}
	return out
}
