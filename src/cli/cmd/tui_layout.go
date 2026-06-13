package cmd

import "github.com/charmbracelet/bubbles/table"

// Fallback terminal dimensions used to lay a TUI out before the first
// tea.WindowSizeMsg arrives (and in tests that build a model directly).
const (
	tuiDefaultWidth  = 100
	tuiDefaultHeight = 30
)

// tuiMinTableHeight is the smallest table height we will ever render: a 2-line
// header plus a handful of body rows, so a tiny terminal still shows something.
const tuiMinTableHeight = 6

// tuiColFloor is the narrowest a column may be shrunk to when even the column
// minimums do not fit the terminal width.
const tuiColFloor = 4

// tuiColSpec describes one table column for responsive layout. Min is the
// smallest acceptable content width; Flex > 0 lets the column share leftover
// horizontal space proportionally (Flex == 0 pins the column at Min).
type tuiColSpec struct {
	Title string
	Min   int
	Flex  int
}

// tuiFitColumns distributes a terminal of termWidth columns across specs,
// giving every column at least its Min and sharing the remaining width among
// flexible columns by weight. The bubbles table adds two columns of cell
// padding per column and the surrounding box border adds two more, all of
// which is reserved here so the table fills the width without overflowing.
func tuiFitColumns(specs []tuiColSpec, termWidth int) []table.Column {
	n := len(specs)
	widths := make([]int, n)
	used, flexTotal := 0, 0
	for i, s := range specs {
		widths[i] = s.Min
		used += s.Min
		flexTotal += s.Flex
	}

	// 2 padding columns per table column + 2 for the box border + 1 safety.
	avail := termWidth - (2*n + 3)
	switch {
	case avail > used && flexTotal > 0:
		// Room to spare: share it among the flexible columns by weight.
		surplus := avail - used
		given, lastFlex := 0, -1
		for i, s := range specs {
			if s.Flex == 0 {
				continue
			}
			add := surplus * s.Flex / flexTotal
			widths[i] += add
			given += add
			lastFlex = i
		}
		if lastFlex >= 0 {
			widths[lastFlex] += surplus - given // absorb the rounding remainder
		}
	case avail < used:
		// Too narrow even for the minimums: shrink the widest column one cell
		// at a time (down to a small floor) so the table still fits the width.
		for deficit := used - avail; deficit > 0; deficit-- {
			widest := -1
			for i := range widths {
				if widths[i] > tuiColFloor && (widest < 0 || widths[i] > widths[widest]) {
					widest = i
				}
			}
			if widest < 0 {
				break // every column is already at the floor
			}
			widths[widest]--
		}
	}

	cols := make([]table.Column, n)
	for i, s := range specs {
		cols[i] = table.Column{Title: s.Title, Width: widths[i]}
	}
	return cols
}

// tuiTableHeight returns the table height (total, including its 2-line header)
// that fills a terminal of termHeight lines after reserving `chrome` lines for
// the surrounding title, box border, and help text.
func tuiTableHeight(termHeight, chrome int) int {
	if h := termHeight - chrome; h > tuiMinTableHeight {
		return h
	}
	return tuiMinTableHeight
}

// Chrome line counts for the table views: a plain list is title + box border
// (2) + help + a one-line bottom margin; a detail view adds a meta line. The
// margin gives the help line room to wrap to a second row on a narrow terminal
// without pushing the layout past the bottom of the screen.
const (
	tuiListChrome   = 5
	tuiDetailChrome = 6
)

// tuiHelp renders a help/hint line, wrapping it to the terminal width so the
// long key listings never overflow a narrow terminal. A non-positive width
// (before the first WindowSizeMsg) renders unwrapped.
func tuiHelp(text string, width int) string {
	if width > 0 {
		return tuiHelpStyle.Width(width).Render(text)
	}
	return tuiHelpStyle.Render(text)
}
