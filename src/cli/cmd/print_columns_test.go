package cmd

import (
	"testing"
)

func hasColumn(cols []string, want string) bool {
	for _, c := range cols {
		if c == want {
			return true
		}
	}
	return false
}

// expires_at is the one timestamp that is status rather than metadata: on a scoped
// token, an invite, or a brokered git credential it decides whether the record still
// works at all. It used to be filtered out with every other _at field, so `armory auth
// token list` would happily print a credential that had already stopped
// authenticating, showing nothing that hinted why. CI checkouts broke daily on exactly
// that blind spot.
func TestTableColumnsShowsExpiry(t *testing.T) {
	// The shape gatekeeper returns from GET /gatekeeper/tokens.
	token := map[string]any{
		"token_id":     "2db3ce54-d2c2-4835-93cb-1a286e36845f",
		"name":         "ci-git-clone",
		"created_at":   "2026-07-28T17:40:16Z",
		"expires_at":   "2027-07-28T17:40:16Z",
		"last_used_at": "2026-07-28T17:40:33Z",
		"expired":      false,
	}

	t.Run("shown by default", func(t *testing.T) {
		defer func(v bool) { flagVerbose = v }(flagVerbose)
		flagVerbose = false

		cols := tableColumns(token)
		if !hasColumn(cols, "expires_at") {
			t.Errorf("expires_at missing from default columns %v — an expired credential would look healthy", cols)
		}
	})

	t.Run("still shown in verbose", func(t *testing.T) {
		defer func(v bool) { flagVerbose = v }(flagVerbose)
		flagVerbose = true

		cols := tableColumns(token)
		if !hasColumn(cols, "expires_at") {
			t.Errorf("expires_at missing from verbose columns %v", cols)
		}
	})

	// The surrounding rule has to stay intact: expiry is the exception, not the start
	// of showing every timestamp. created_at stays verbose-only and the noisier
	// per-row timestamps stay hidden in both modes, or list output becomes unreadable.
	t.Run("other timestamps keep their existing treatment", func(t *testing.T) {
		defer func(v bool) { flagVerbose = v }(flagVerbose)

		flagVerbose = false
		cols := tableColumns(token)
		if hasColumn(cols, "created_at") {
			t.Errorf("created_at leaked into default columns %v", cols)
		}
		if hasColumn(cols, "last_used_at") {
			t.Errorf("last_used_at leaked into default columns %v", cols)
		}

		flagVerbose = true
		cols = tableColumns(token)
		if !hasColumn(cols, "created_at") {
			t.Errorf("created_at missing from verbose columns %v", cols)
		}
		if hasColumn(cols, "last_used_at") {
			t.Errorf("last_used_at should stay hidden even in verbose, got %v", cols)
		}
	})

	// A row with no expiry must not grow an empty column.
	t.Run("absent on records that do not expire", func(t *testing.T) {
		defer func(v bool) { flagVerbose = v }(flagVerbose)
		flagVerbose = false

		cols := tableColumns(map[string]any{
			"name":       "some-board",
			"created_at": "2026-07-28T17:40:16Z",
		})
		if hasColumn(cols, "expires_at") {
			t.Errorf("expires_at invented for a record without one: %v", cols)
		}
	})
}
