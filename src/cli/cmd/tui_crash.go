package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// runTUIProgram runs a bubbletea program and, if it panics, captures the panic value
// and stack to a crash-log file so an otherwise-vanishing TUI panic can be diagnosed.
// bubbletea restores the terminal on panic and re-panics; without this wrapper that
// stack trace lands on a screen the alt-screen teardown has often already cleared,
// leaving a bug report of just "the TUI crashed". The returned error names the log so
// the user can attach it. The panic is deliberately not re-raised: a clean error to
// the CLI beats a raw stack dump to a half-restored terminal.
func runTUIProgram(p *tea.Program) (err error) {
	defer func() {
		if r := recover(); r != nil {
			path := writeTUICrashLog(r, debug.Stack())
			err = fmt.Errorf("the TUI crashed: %v\na crash report was written to %s — please attach it when reporting this", r, path)
		}
	}()
	_, err = p.Run()
	return err
}

// writeTUICrashLog persists a panic and its stack to a file in the OS temp dir,
// returning the path (or a short diagnostic if the write itself failed).
func writeTUICrashLog(r any, stack []byte) string {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("armory-tui-crash-%d.log", time.Now().UnixNano()))
	body := fmt.Sprintf("armory TUI crash\npanic: %v\n\n%s", r, stack)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "(failed to write crash log: " + err.Error() + ")"
	}
	return path
}
