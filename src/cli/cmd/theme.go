package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// colorTheme is the semantic palette shared by every TUI. Fields are hex color
// strings; concrete lipgloss styles are derived from them in buildStyles().
type colorTheme struct {
	Accent  string // primary highlight: focus, titles, cursor, success
	Text    string // primary body text
	Muted   string // secondary text: help, meta, field labels
	Subtle  string // dim text: subtitles, disabled, low priority
	Medium  string // mid-tone: medium priority, dimmed descriptions
	Border  string // inactive borders
	SelBg   string // background of the selected table row
	Warning string // running state, high priority
	Danger  string // errors, critical, failed
}

// themes is the registry of selectable palettes. cyber is the original look and
// the default; tokyo-night is a cool blue dark theme; light-cyber is a bright,
// high-key variant of cyber (near-white text, lighter borders).
var themes = map[string]colorTheme{
	"cyber": {
		Accent:  "#39ff14",
		Text:    "#d6dcd2",
		Muted:   "#a8b4a2",
		Subtle:  "#4a5346",
		Medium:  "#7d8a78",
		Border:  "#1a2018",
		SelBg:   "#050705",
		Warning: "#c9b060",
		Danger:  "#d46b55",
	},
	"tokyo-night": {
		Accent:  "#7aa2f7",
		Text:    "#c0caf5",
		Muted:   "#a9b1d6",
		Subtle:  "#565f89",
		Medium:  "#737aa2",
		Border:  "#292e42",
		SelBg:   "#283457",
		Warning: "#e0af68",
		Danger:  "#f7768e",
	},
	"light-cyber": {
		Accent:  "#8dff7a",
		Text:    "#f4f8f2",
		Muted:   "#cdd8c8",
		Subtle:  "#9fad99",
		Medium:  "#b6c2b0",
		Border:  "#6f7e68",
		SelBg:   "#243a1e",
		Warning: "#ffd75f",
		Danger:  "#ff8678",
	},
	"dracula": {
		Accent:  "#bd93f9",
		Text:    "#f8f8f2",
		Muted:   "#bcc0d4",
		Subtle:  "#6272a4",
		Medium:  "#7e88b8",
		Border:  "#44475a",
		SelBg:   "#44475a",
		Warning: "#ffb86c",
		Danger:  "#ff5555",
	},
	"nord": {
		Accent:  "#88c0d0",
		Text:    "#eceff4",
		Muted:   "#d8dee9",
		Subtle:  "#7b88a1",
		Medium:  "#9aa5b8",
		Border:  "#434c5e",
		SelBg:   "#3b4252",
		Warning: "#ebcb8b",
		Danger:  "#bf616a",
	},
	"gruvbox": {
		Accent:  "#b8bb26",
		Text:    "#ebdbb2",
		Muted:   "#d5c4a1",
		Subtle:  "#928374",
		Medium:  "#a89984",
		Border:  "#504945",
		SelBg:   "#3c3836",
		Warning: "#fabd2f",
		Danger:  "#fb4934",
	},
	"catppuccin": {
		Accent:  "#cba6f7",
		Text:    "#cdd6f4",
		Muted:   "#bac2de",
		Subtle:  "#6c7086",
		Medium:  "#a6adc8",
		Border:  "#45475a",
		SelBg:   "#313244",
		Warning: "#f9e2af",
		Danger:  "#f38ba8",
	},
	"solarized": {
		Accent:  "#268bd2",
		Text:    "#93a1a1",
		Muted:   "#839496",
		Subtle:  "#586e75",
		Medium:  "#657b83",
		Border:  "#073642",
		SelBg:   "#073642",
		Warning: "#b58900",
		Danger:  "#dc322f",
	},
}

// themeOrder is the display order for `armory theme list` (maps are unordered).
var themeOrder = []string{
	"cyber", "tokyo-night", "light-cyber",
	"dracula", "nord", "gruvbox", "catppuccin", "solarized",
}

const defaultTheme = "cyber"

// activeTheme / activeThemeName hold the resolved palette. They default to the
// built-in theme so package-var styles have valid colors before resolution.
var (
	activeTheme     = themes[defaultTheme]
	activeThemeName = defaultTheme
	flagTheme       string
)

// normalizeTheme lowercases a theme name and accepts a few friendly aliases so
// "tokyonight", "tokyo night", and "light" all resolve to a canonical key.
func normalizeTheme(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "tokyonight", "tokyo_night", "tokyo night", "tokyo":
		return "tokyo-night"
	case "light", "lightcyber", "light_cyber", "light cyber", "bright":
		return "light-cyber"
	case "dark", "default":
		return "cyber"
	case "catppuccin-mocha", "mocha":
		return "catppuccin"
	case "solarized-dark", "solarized dark":
		return "solarized"
	case "gruvbox-dark", "gruvbox dark":
		return "gruvbox"
	}
	return n
}

// resolveThemeName picks a theme by precedence: --theme flag, CODEARMORY_THEME
// env var, then the saved config value. Unknown names fall through to the next
// source and ultimately to defaultTheme, so a typo never leaves the UI unstyled.
func resolveThemeName(flag, env, cfgVal string) string {
	for _, candidate := range []string{flag, env, cfgVal} {
		if candidate == "" {
			continue
		}
		if _, ok := themes[normalizeTheme(candidate)]; ok {
			return normalizeTheme(candidate)
		}
	}
	return defaultTheme
}

// setActiveTheme switches the active palette by name and rebuilds every TUI
// style. The name must be a registered theme key (callers pass values from
// themeOrder or resolveThemeName); unknown names are ignored.
func setActiveTheme(name string) {
	t, ok := themes[name]
	if !ok {
		return
	}
	activeThemeName = name
	activeTheme = t
	buildStyles()
}

// applyResolvedTheme resolves the active theme from flag/env/config and rebuilds
// every TUI style. It runs from rootCmd's PersistentPreRun (so the --theme flag
// is parsed first) and once at init so styles are valid even outside cobra.
func applyResolvedTheme() {
	setActiveTheme(resolveThemeName(flagTheme, os.Getenv("CODEARMORY_THEME"), loadConfig().Theme))
}

// buildStyles rebuilds all package-level TUI styles from activeTheme. Each TUI
// file owns a builder for the styles declared there; this is the single entry
// point that fans out to them whenever the active theme changes.
func buildStyles() {
	buildHomeStyles()
	buildCITUIStyles()
	buildBoardStyles()
	buildBoardFormStyles()
	buildTuiFormStyles()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagTheme, "theme", "", "color theme (see 'armory theme list'; overrides CODEARMORY_THEME and config)")
	// Resolve once at init so env/config are honored even for code paths that
	// bypass cobra (e.g. tests); PersistentPreRun re-resolves with the flag.
	applyResolvedTheme()
	rootCmd.PersistentPreRun = func(cmd *cobra.Command, args []string) { applyResolvedTheme() }

	RegisterModule(Module{Name: "theme", Command: newThemeCmd()})
}

// newThemeCmd builds the `armory theme` command group.
func newThemeCmd() *cobra.Command {
	themeCmd := &cobra.Command{
		Use:   "theme",
		Short: "View and switch the CLI color theme",
		Long: `Manage the color theme used by the interactive TUIs.

Available themes:
  cyber        neon-green on black (default)
  tokyo-night  cool blue dark theme
  light-cyber  bright, high-key variant of cyber
  dracula      purple/pink on dark slate
  nord         icy blue-gray
  gruvbox      warm retro green/amber
  catppuccin   soft mauve pastel (mocha)
  solarized    classic solarized dark

Run "armory theme list" to preview, or "armory settings" for an interactive picker.
Precedence (highest first): --theme flag, CODEARMORY_THEME env var, saved config.`,
		// Default action lists themes.
		RunE: func(cmd *cobra.Command, args []string) error { return runThemeList(cmd) },
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List available themes",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runThemeList(cmd) },
	}

	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Show the active theme",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), activeThemeName)
			return nil
		},
	}

	setCmd := &cobra.Command{
		Use:   "set <theme>",
		Short: "Save the theme to config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := normalizeTheme(args[0])
			if _, ok := themes[name]; !ok {
				return fmt.Errorf("unknown theme %q (choose one of: %s)", args[0], strings.Join(themeOrder, ", "))
			}
			cfg := loadConfig()
			cfg.Theme = name
			if err := saveConfig(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Theme set to %s.\n", name)
			return nil
		},
	}

	themeCmd.AddCommand(listCmd, showCmd, setCmd)
	return themeCmd
}

// runThemeList prints every theme, swatching its accent color and marking the
// active one.
func runThemeList(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	for _, name := range themeOrder {
		t := themes[name]
		marker := "  "
		if name == activeThemeName {
			marker = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Accent)).Render("▶ ")
		}
		swatch := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Accent)).Render("●")
		label := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Text)).Render(name)
		fmt.Fprintf(out, "%s%s %s\n", marker, swatch, label)
	}
	return nil
}
