package cmd

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// tuiForm is a vertical stack of labelled text inputs with focus navigation,
// shared by the create dialogs in the forge, CI and hooks TUIs. The board has
// its own bespoke form (board_form.go) because its fields include cycling
// selectors and a textarea; this lighter component covers the common
// single-line "fill in these fields and submit" case.

// fieldKind distinguishes the field controls a tuiForm supports: a single-line
// text input, a multi-line text area, a ←/→ option cycler, and the submit button.
type fieldKind int

const (
	fieldText     fieldKind = iota // single-line textinput
	fieldSelect                    // ←/→ cycling selector
	fieldTextarea                  // multi-line textarea (commands, JSON, prose)
	fieldButton                    // submit button (no value)
)

// formField is one labelled control within a tuiForm.
type formField struct {
	key     string // identifier used to read the value back
	label   string
	kind    fieldKind
	input   textinput.Model // fieldText only
	area    textarea.Model  // fieldTextarea only
	options []string        // fieldSelect: display labels
	values  []string        // fieldSelect: values read back, parallel to options (nil → read back the label)
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

// formTextarea builds a labelled multi-line field for content that is naturally
// multi-line — shell commands, JSON, prose. Enter inserts a newline here (the
// form submits via its button / ctrl+s, not Enter).
func formTextarea(key, label, placeholder string) formField {
	return formField{key: key, label: label, kind: fieldTextarea, area: newFormTextarea(placeholder)}
}

// formButton builds a non-value control that submits the form when activated
// (Enter or Space while focused).
func formButton(label string) formField {
	return formField{kind: fieldButton, label: label}
}

// newFormTextarea builds a themed multi-line input mirroring newFormInput's look.
func newFormTextarea(placeholder string) textarea.Model {
	ta := textarea.New()
	ta.Placeholder = placeholder
	ta.ShowLineNumbers = false
	ta.SetWidth(tuiFormInputW)
	ta.SetHeight(tuiFormTextareaH)
	// Drop the cursor-line highlight so it doesn't paint a background stripe.
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Base = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Text))
	ta.FocusedStyle.Text = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Text))
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	ta.BlurredStyle.Base = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	ta.BlurredStyle.Text = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	ta.BlurredStyle.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Muted))
	ta.Cursor.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Accent))
	return ta
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

// formSelectKV builds a selector that displays labels but reads back the
// parallel value at the chosen index. Use it when the human-friendly label
// (e.g. a pipeline or role name) differs from the value submitted to the API
// (e.g. its id); labels and values must be the same length.
func formSelectKV(key, label string, labels, values []string) formField {
	return formField{key: key, label: label, kind: fieldSelect, options: labels, values: values}
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

// newTUIForm builds a form and focuses its first field. A submit button is always
// appended as the final focusable control, so Enter need not submit — letting
// multi-line fields use Enter for newlines. The returned tea.Cmd starts the cursor
// blink on the first field.
func newTUIForm(title string, fields ...formField) (tuiForm, tea.Cmd) {
	fields = append(fields, formButton("Submit"))
	f := tuiForm{title: title, fields: fields}
	return f, f.focusActive()
}

// value returns the value of the field with the given key (or ""). Text values
// are trimmed; a plain select reads back its chosen option (an empty option →
// ""), while a key/value select (formSelectKV) reads back the parallel value at
// the chosen index — the id behind a displayed name.
func (f tuiForm) value(key string) string {
	for i := range f.fields {
		fld := f.fields[i]
		if fld.key != key {
			continue
		}
		switch fld.kind {
		case fieldSelect:
			if fld.sel < 0 || fld.sel >= len(fld.options) {
				return ""
			}
			if fld.values != nil && fld.sel < len(fld.values) {
				return fld.values[fld.sel]
			}
			return fld.options[fld.sel]
		case fieldTextarea:
			return strings.TrimSpace(fld.area.Value())
		case fieldButton:
			return ""
		default:
			return strings.TrimSpace(fld.input.Value())
		}
	}
	return ""
}

// setValue sets a field's value: a selector selects the matching option (a
// key/value select matches against its parallel value list — the id — a plain
// select against its visible options), leaving the current choice if the value
// isn't found; a text field replaces its contents. Used when rebuilding a form
// against a catalog that arrived after the form was opened.
func (f *formField) setValue(val string) {
	switch f.kind {
	case fieldSelect:
		haystack := f.options
		if f.values != nil {
			haystack = f.values
		}
		for i, o := range haystack {
			if o == val {
				f.sel = i
				return
			}
		}
	case fieldTextarea:
		f.area.SetValue(val)
	case fieldButton:
		// no value
	default:
		f.input.SetValue(val)
	}
}

// focusActive focuses the active text/textarea field, blurs the rest, and returns
// the cursor blink cmd. Select and button fields take no text focus.
func (f *tuiForm) focusActive() tea.Cmd {
	var cmd tea.Cmd
	for i := range f.fields {
		switch f.fields[i].kind {
		case fieldText:
			if i == f.focus {
				cmd = f.fields[i].input.Focus()
			} else {
				f.fields[i].input.Blur()
			}
		case fieldTextarea:
			if i == f.focus {
				cmd = f.fields[i].area.Focus()
			} else {
				f.fields[i].area.Blur()
			}
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
//
// Submission is via the Submit button (Enter/Space while it is focused) or the
// ctrl+s shortcut — never a bare Enter, so multi-line fields can use Enter for
// newlines. Tab/shift+tab always move between fields; arrow up/down move between
// fields too, except inside a textarea where they move the cursor. ctrl+c is
// intentionally not handled here — callers quit on it before delegating.
func (f tuiForm) update(msg tea.Msg) (tuiForm, formAction, tea.Cmd) {
	// Copy the field slice so the returned form never aliases the caller's
	// backing array (bubbletea passes models by value).
	fields := make([]formField, len(f.fields))
	copy(fields, f.fields)
	f.fields = fields

	if len(f.fields) == 0 {
		return f, formNone, nil
	}

	if key, ok := msg.(tea.KeyMsg); ok {
		cur := f.fields[f.focus].kind
		switch key.String() {
		case "esc":
			return f, formCancel, nil
		case "ctrl+s":
			return f, formSubmit, nil
		case "tab":
			f.focus = (f.focus + 1) % len(f.fields)
			return f, formNone, f.focusActive()
		case "shift+tab":
			f.focus = (f.focus - 1 + len(f.fields)) % len(f.fields)
			return f, formNone, f.focusActive()
		case "down":
			if cur == fieldTextarea {
				break // let the textarea move its cursor
			}
			f.focus = (f.focus + 1) % len(f.fields)
			return f, formNone, f.focusActive()
		case "up":
			if cur == fieldTextarea {
				break
			}
			f.focus = (f.focus - 1 + len(f.fields)) % len(f.fields)
			return f, formNone, f.focusActive()
		case "enter":
			switch cur {
			case fieldButton:
				return f, formSubmit, nil
			case fieldTextarea:
				break // insert a newline (handled by the delegate below)
			default:
				// Single-line fields advance toward the Submit button.
				f.focus = (f.focus + 1) % len(f.fields)
				return f, formNone, f.focusActive()
			}
		case " ":
			if cur == fieldButton {
				return f, formSubmit, nil
			}
		case "left":
			if cur == fieldSelect {
				f.cycle(-1)
				return f, formNone, nil
			}
		case "right":
			if cur == fieldSelect {
				f.cycle(+1)
				return f, formNone, nil
			}
		}
	}

	// Delegate the message to the focused control. Selects and the button take no
	// text input.
	switch f.fields[f.focus].kind {
	case fieldSelect, fieldButton:
		return f, formNone, nil
	case fieldTextarea:
		var cmd tea.Cmd
		f.fields[f.focus].area, cmd = f.fields[f.focus].area.Update(msg)
		return f, formNone, cmd
	default:
		var cmd tea.Cmd
		f.fields[f.focus].input, cmd = f.fields[f.focus].input.Update(msg)
		return f, formNone, cmd
	}
}

// ── Styles ────────────────────────────────────────────────────────────────────

const (
	tuiFormLabelW    = 12
	tuiFormInputW    = 36
	tuiFormTextareaH = 4 // visible rows for multi-line (textarea) fields
)

var (
	tuiFormLabel        lipgloss.Style
	tuiFormLabelActive  lipgloss.Style
	tuiFormBox          lipgloss.Style
	tuiFormHeading      lipgloss.Style
	tuiFormCycle        lipgloss.Style
	tuiFormHint         lipgloss.Style
	tuiFormButton       lipgloss.Style
	tuiFormButtonActive lipgloss.Style
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

	tuiFormButton = lipgloss.NewStyle().
		Foreground(lipgloss.Color(activeTheme.Muted)).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(activeTheme.Border)).
		Padding(0, 2)

	tuiFormButtonActive = lipgloss.NewStyle().
		Foreground(lipgloss.Color(activeTheme.Accent)).
		Bold(true).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(activeTheme.Accent)).
		Padding(0, 2)
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
	hasSelect, hasTextarea := false, false
	for i := range f.fields {
		// The submit button spans the full row (no label column), set off by a
		// blank line, highlighted when focused.
		if f.fields[i].kind == fieldButton {
			btnStyle := tuiFormButton
			if i == f.focus {
				btnStyle = tuiFormButtonActive
			}
			rows = append(rows, "", lipgloss.JoinHorizontal(lipgloss.Top,
				tuiFormLabel.Render(""), btnStyle.Render(f.fields[i].label)))
			continue
		}

		lbl := tuiFormLabel
		if i == f.focus {
			lbl = tuiFormLabelActive
		}
		var control string
		switch f.fields[i].kind {
		case fieldSelect:
			hasSelect = true
			control = tuiFormCycle.Render("‹ " + f.fields[i].selectDisplay() + " ›")
		case fieldTextarea:
			hasTextarea = true
			ta := f.fields[i].area
			ta.SetWidth(tuiFormInputW)
			control = ta.View()
		default:
			in := f.fields[i].input
			in.Width = tuiFormInputW
			control = in.View()
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, lbl.Render(f.fields[i].label), control))
	}
	if f.errMsg != "" {
		rows = append(rows, "", tuiErrStyle.Render(f.errMsg))
	}

	parts := []string{"tab: move"}
	if hasSelect {
		parts = append(parts, "←/→: options")
	}
	if hasTextarea {
		parts = append(parts, "enter: newline")
	}
	parts = append(parts, "Submit / ctrl+s: submit", "esc: cancel")
	rows = append(rows, "", tuiFormHint.Render(strings.Join(parts, "   ")))

	box := tuiFormBox.Render(strings.Join(rows, "\n"))
	if width > 0 && height > 0 {
		return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
	}
	return "\n" + box
}
