package cmd

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// tuiForm is a vertical stack of labelled text inputs with focus navigation,
// shared by the create dialogs in the forge, CI and hooks TUIs. The board has
// its own bespoke form (board_form.go) because its fields include cycling
// selectors and a textarea; this lighter component covers the common
// single-line "fill in these fields and submit" case.

// fieldKind distinguishes a free-text input from a fixed list of options the
// user cycles through with ←/→.
type fieldKind int

const (
	fieldText fieldKind = iota
	fieldSelect
)

// formField is one labelled field within a tuiForm: either a text input
// (fieldText) or a ←/→ cycling selector over a fixed option list (fieldSelect).
type formField struct {
	key     string // identifier used to read the value back
	label   string
	kind    fieldKind
	input   textinput.Model // fieldText only
	options []string        // fieldSelect only
	sel     int             // fieldSelect: index into options
}

// formInput builds a labelled text field with the given placeholder.
func formInput(key, label, placeholder string) formField {
	return formField{key: key, label: label, kind: fieldText, input: newFormInput(placeholder)}
}

// formInputDefault builds a labelled text field pre-filled with value.
func formInputDefault(key, label, placeholder, value string) formField {
	f := formInput(key, label, placeholder)
	f.input.SetValue(value)
	return f
}

// formSelect builds a labelled selector that cycles through options with ←/→.
// An empty-string option renders as "(default)" and reads back as "".
func formSelect(key, label string, options []string) formField {
	return formField{key: key, label: label, kind: fieldSelect, options: options}
}

// formSelectDefault builds a selector with the option equal to def pre-selected.
func formSelectDefault(key, label string, options []string, def string) formField {
	f := formSelect(key, label, options)
	for i, o := range options {
		if o == def {
			f.sel = i
			break
		}
	}
	return f
}

// formAction is the outcome of feeding a key to a tuiForm.
type formAction int

const (
	formNone formAction = iota
	formSubmit
	formCancel
)

type tuiForm struct {
	title  string
	fields []formField
	focus  int
	errMsg string // inline validation / submission error
}

// newTUIForm builds a form and focuses its first field. The returned tea.Cmd
// starts the cursor blink on that field.
func newTUIForm(title string, fields ...formField) (tuiForm, tea.Cmd) {
	f := tuiForm{title: title, fields: fields}
	return f, f.focusActive()
}

// value returns the value of the field with the given key (or ""). Text values
// are trimmed; select values are the chosen option (an empty option → "").
func (f tuiForm) value(key string) string {
	for i := range f.fields {
		if f.fields[i].key != key {
			continue
		}
		if f.fields[i].kind == fieldSelect {
			if f.fields[i].sel >= 0 && f.fields[i].sel < len(f.fields[i].options) {
				return f.fields[i].options[f.fields[i].sel]
			}
			return ""
		}
		return strings.TrimSpace(f.fields[i].input.Value())
	}
	return ""
}

// focusActive focuses the active text field, blurs the rest, and returns the
// cursor blink cmd. Select fields take no text focus.
func (f *tuiForm) focusActive() tea.Cmd {
	var cmd tea.Cmd
	for i := range f.fields {
		if f.fields[i].kind != fieldText {
			continue
		}
		if i == f.focus {
			cmd = f.fields[i].input.Focus()
		} else {
			f.fields[i].input.Blur()
		}
	}
	return cmd
}

// cycle advances the focused select field's choice by delta (wrapping).
func (f *tuiForm) cycle(delta int) {
	fld := &f.fields[f.focus]
	if fld.kind != fieldSelect || len(fld.options) == 0 {
		return
	}
	n := len(fld.options)
	fld.sel = (fld.sel + delta + n) % n
}

// update feeds a message to the form, returning the new form, the action the
// user requested (submit/cancel/none), and any cmd from the active input.
// ctrl+c is intentionally not handled here — callers quit on it before
// delegating, so it never lands in a text field.
func (f tuiForm) update(msg tea.Msg) (tuiForm, formAction, tea.Cmd) {
	// Copy the field slice so the returned form never aliases the caller's
	// backing array (bubbletea passes models by value).
	fields := make([]formField, len(f.fields))
	copy(fields, f.fields)
	f.fields = fields

	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			return f, formCancel, nil
		case "enter", "ctrl+s":
			return f, formSubmit, nil
		case "tab", "down":
			f.focus = (f.focus + 1) % len(f.fields)
			return f, formNone, f.focusActive()
		case "shift+tab", "up":
			f.focus = (f.focus - 1 + len(f.fields)) % len(f.fields)
			return f, formNone, f.focusActive()
		case "left":
			if len(f.fields) > 0 && f.fields[f.focus].kind == fieldSelect {
				f.cycle(-1)
				return f, formNone, nil
			}
		case "right":
			if len(f.fields) > 0 && f.fields[f.focus].kind == fieldSelect {
				f.cycle(+1)
				return f, formNone, nil
			}
		}
	}

	if len(f.fields) == 0 {
		return f, formNone, nil
	}
	// Select fields have no text input; they only respond to ←/→ (handled above).
	if f.fields[f.focus].kind == fieldSelect {
		return f, formNone, nil
	}
	var cmd tea.Cmd
	f.fields[f.focus].input, cmd = f.fields[f.focus].input.Update(msg)
	return f, formNone, cmd
}

// ── Styles ────────────────────────────────────────────────────────────────────

const (
	tuiFormLabelW = 12
	tuiFormInputW = 36
)

var (
	tuiFormLabel       lipgloss.Style
	tuiFormLabelActive lipgloss.Style
	tuiFormBox         lipgloss.Style
	tuiFormHeading     lipgloss.Style
	tuiFormCycle       lipgloss.Style
	tuiFormHint        lipgloss.Style
)

// buildTuiFormStyles rebuilds the shared TUI form styles from the active theme.
func buildTuiFormStyles() {
	tuiFormLabel = lipgloss.NewStyle().
		Foreground(lipgloss.Color(activeTheme.Muted)).
		Width(tuiFormLabelW)

	tuiFormLabelActive = lipgloss.NewStyle().
		Foreground(lipgloss.Color(activeTheme.Text)).
		Width(tuiFormLabelW)

	tuiFormBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(activeTheme.Accent)).
		Padding(1, 3).
		Width(58)

	tuiFormHeading = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(activeTheme.Accent)).
		MarginBottom(1)

	tuiFormCycle = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Text))
	tuiFormHint = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
}

// selectDisplay renders a select field's current choice; an empty option (and
// an empty option list) reads as "(default)".
func (fld formField) selectDisplay() string {
	if len(fld.options) == 0 {
		return "(default)"
	}
	v := fld.options[fld.sel]
	if v == "" {
		return "(default)"
	}
	return v
}

// view renders the form centered within the given terminal dimensions.
func (f tuiForm) view(width, height int) string {
	rows := []string{tuiFormHeading.Render(f.title)}
	hasSelect := false
	for i := range f.fields {
		lbl := tuiFormLabel
		if i == f.focus {
			lbl = tuiFormLabelActive
		}
		var control string
		if f.fields[i].kind == fieldSelect {
			hasSelect = true
			control = tuiFormCycle.Render("‹ " + f.fields[i].selectDisplay() + " ›")
		} else {
			in := f.fields[i].input
			in.Width = tuiFormInputW
			control = in.View()
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, lbl.Render(f.fields[i].label), control))
	}
	if f.errMsg != "" {
		rows = append(rows, "", tuiErrStyle.Render(f.errMsg))
	}
	hint := "tab: next field   enter/ctrl+s: submit   esc: cancel"
	if hasSelect {
		hint = "tab: next field   ←/→: cycle options   enter/ctrl+s: submit   esc: cancel"
	}
	rows = append(rows, "", tuiFormHint.Render(hint))

	box := tuiFormBox.Render(strings.Join(rows, "\n"))
	if width > 0 && height > 0 {
		return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
	}
	return "\n" + box
}
