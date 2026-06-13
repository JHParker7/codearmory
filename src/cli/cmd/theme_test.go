package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// restoreTheme resets the active theme back to the default after a test mutates
// it, so style-dependent tests don't leak state between cases.
func restoreTheme(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		activeThemeName = defaultTheme
		activeTheme = themes[defaultTheme]
		buildStyles()
	})
}

func TestThemesArePopulated(t *testing.T) {
	for name, th := range themes {
		fields := map[string]string{
			"Accent": th.Accent, "Text": th.Text, "Muted": th.Muted,
			"Subtle": th.Subtle, "Medium": th.Medium, "Border": th.Border,
			"SelBg": th.SelBg, "Warning": th.Warning, "Danger": th.Danger,
		}
		for field, val := range fields {
			if !strings.HasPrefix(val, "#") || len(val) != 7 {
				t.Errorf("theme %q field %s = %q, want a #rrggbb hex color", name, field, val)
			}
		}
	}
}

func TestThemeOrderMatchesRegistry(t *testing.T) {
	if len(themeOrder) != len(themes) {
		t.Fatalf("themeOrder has %d entries, themes has %d", len(themeOrder), len(themes))
	}
	for _, name := range themeOrder {
		if _, ok := themes[name]; !ok {
			t.Errorf("themeOrder lists %q which is not in themes", name)
		}
	}
	if _, ok := themes[defaultTheme]; !ok {
		t.Errorf("defaultTheme %q is not a registered theme", defaultTheme)
	}
}

func TestResolveThemeName(t *testing.T) {
	cases := []struct {
		name           string
		flag, env, cfg string
		want           string
	}{
		{"all empty -> default", "", "", "", "cyber"},
		{"flag wins", "tokyo-night", "light-cyber", "cyber", "tokyo-night"},
		{"env over config", "", "light-cyber", "cyber", "light-cyber"},
		{"config used last", "", "", "tokyo-night", "tokyo-night"},
		{"invalid flag falls through to env", "bogus", "tokyo-night", "", "tokyo-night"},
		{"all invalid -> default", "nope", "nada", "zilch", "cyber"},
		{"alias tokyonight", "tokyonight", "", "", "tokyo-night"},
		{"alias light", "light", "", "", "light-cyber"},
		{"alias bright", "", "bright", "", "light-cyber"},
		{"case insensitive", "TOKYO-NIGHT", "", "", "tokyo-night"},
		{"whitespace trimmed", "  cyber  ", "", "", "cyber"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveThemeName(c.flag, c.env, c.cfg); got != c.want {
				t.Errorf("resolveThemeName(%q,%q,%q) = %q, want %q", c.flag, c.env, c.cfg, got, c.want)
			}
		})
	}
}

func TestBuildStylesAppliesTheme(t *testing.T) {
	restoreTheme(t)
	for _, name := range themeOrder {
		activeThemeName = name
		activeTheme = themes[name]
		buildStyles()

		want := lipgloss.Color(themes[name].Accent)
		if got := tuiTitleStyle.GetForeground(); got != want {
			t.Errorf("%s: tuiTitleStyle foreground = %v, want %v", name, got, want)
		}
		if got := tuiStatusColors["completed"]; got != want {
			t.Errorf("%s: completed status color = %v, want %v", name, got, want)
		}
		// Default priority swatches must follow the theme too.
		if len(defaultBoardPriorities) == 0 {
			t.Fatalf("%s: defaultBoardPriorities not populated", name)
		}
		if defaultBoardPriorities[3].Color != themes[name].Danger {
			t.Errorf("%s: critical priority color = %q, want %q", name, defaultBoardPriorities[3].Color, themes[name].Danger)
		}
	}
}

func TestThemeSetPersistsToConfig(t *testing.T) {
	isolateHome(t)
	restoreTheme(t)

	themeCmd := newThemeCmd()
	setCmd, _, err := themeCmd.Find([]string{"set"})
	if err != nil {
		t.Fatalf("find set: %v", err)
	}
	var buf bytes.Buffer
	setCmd.SetOut(&buf)

	if err := setCmd.RunE(setCmd, []string{"tokyonight"}); err != nil {
		t.Fatalf("theme set: %v", err)
	}
	if got := loadConfig().Theme; got != "tokyo-night" {
		t.Errorf("saved theme = %q, want tokyo-night (alias should be normalized)", got)
	}
	if !strings.Contains(buf.String(), "tokyo-night") {
		t.Errorf("set output = %q, want confirmation mentioning tokyo-night", buf.String())
	}
}

func TestThemeSetRejectsUnknown(t *testing.T) {
	isolateHome(t)
	themeCmd := newThemeCmd()
	setCmd, _, err := themeCmd.Find([]string{"set"})
	if err != nil {
		t.Fatalf("find set: %v", err)
	}
	if err := setCmd.RunE(setCmd, []string{"neon-pink"}); err == nil {
		t.Error("theme set with unknown name should error")
	}
	if got := loadConfig().Theme; got != "" {
		t.Errorf("config theme = %q, want empty after a rejected set", got)
	}
}

func TestThemeListShowsAllThemes(t *testing.T) {
	themeCmd := newThemeCmd()
	var buf bytes.Buffer
	themeCmd.SetOut(&buf)
	if err := runThemeList(themeCmd); err != nil {
		t.Fatalf("list: %v", err)
	}
	out := buf.String()
	for _, name := range themeOrder {
		if !strings.Contains(out, name) {
			t.Errorf("theme list output missing %q:\n%s", name, out)
		}
	}
}
