package cmd

import "testing"

// The --can shorthand is the part a human types, so its expansion has to be both
// predictable and safe: the two-part form must never widen beyond the caller's own
// namespace, since that is the only place their permissions live.
func TestParsePermissionFlag(t *testing.T) {
	t.Run("parses the full triple", func(t *testing.T) {
		got, err := parsePermissionFlag("tickets:createTicket:alice/tickets/tickets")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["service"] != "tickets" || got["action"] != "createTicket" || got["resource"] != "alice/tickets/tickets" {
			t.Errorf("parsed = %v", got)
		}
	})

	t.Run("keeps an explicit resource verbatim", func(t *testing.T) {
		got, err := parsePermissionFlag("codearmory_git_factory:readRepo:alice/codearmory_git_factory/repos/abc")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "alice/codearmory_git_factory/repos/abc"; got["resource"] != want {
			t.Errorf("resource = %v, want %v", got["resource"], want)
		}
	})

	t.Run("rejects malformed values", func(t *testing.T) {
		// A two-part value is rejected rather than guessed at: an invented resource
		// would be refused by the server anyway, with a far less obvious message.
		for _, raw := range []string{"", "tickets", ":createTicket", "tickets:", "tickets:createTicket", "tickets:createTicket:"} {
			if _, err := parsePermissionFlag(raw); err == nil {
				t.Errorf("parsePermissionFlag(%q) accepted a malformed value", raw)
			}
		}
	})
}
