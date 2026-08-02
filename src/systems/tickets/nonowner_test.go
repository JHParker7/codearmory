package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Does a stranger reach another user's records?
//
// Conductor templates a resource from PATH parameters alone, and gatekeeper then
// prefixes the CALLER's namespace to an unscoped one — so the legacy declaration
// "tickets/tickets/{id}" evaluates to "<caller>/tickets/tickets/<id>" whoever asks.
// Anyone holding a wildcard over their own tickets is therefore authorized at the
// gateway for EVERY id in the system. What actually stops them is each handler
// re-checking ownership, and that is a convention rather than a guarantee — the same
// sweep over git_factory found 19 of 24 per-record routes leaking.
//
// The gatekeeper stub returns AUTHORIZED as a different user on purpose. That is not a
// lenient stub concealing the answer; it is what the real gatekeeper returns here,
// because the stranger's own wildcard matches the caller-prefixed resource. So every
// denial below is the service's doing and nothing else's.

func seedTicketFor(t *testing.T, user, org, ns string) Ticket {
	t.Helper()
	now := time.Now().UTC()
	tk := Ticket{
		TicketID: uuid.New().String(), Title: "victim-" + uuid.New().String(),
		Status: StatusOpen, Priority: PriorityMedium, CreatedBy: user, OrgID: org,
		Namespace: ns, Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := tk.Add(context.Background()); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM tickets WHERE ticket_id = ?`, tk.TicketID) }) //nolint:errcheck
	return tk
}

func seedCommentOn(t *testing.T, ticketID, author string) TicketComment {
	t.Helper()
	now := time.Now().UTC()
	c := TicketComment{
		CommentID: uuid.New().String(), TicketID: ticketID, AuthorID: author,
		Body: "victim-comment", Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := c.Add(context.Background()); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM ticket_comments WHERE comment_id = ?`, c.CommentID) }) //nolint:errcheck
	return c
}

func seedBoardFor(t *testing.T, user, org string) Board {
	t.Helper()
	now := time.Now().UTC()
	b := Board{
		BoardID: uuid.New().String(), Name: "victim-board-" + uuid.New().String(),
		CreatedBy: user, OrgID: org, Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := b.Add(context.Background()); err != nil {
		t.Fatalf("seed board: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM ticket_boards WHERE board_id = ?`, b.BoardID) }) //nolint:errcheck
	return b
}

func seedFieldDefOn(t *testing.T, boardID, orgID string) TicketFieldDef {
	t.Helper()
	now := time.Now().UTC()
	f := TicketFieldDef{
		FieldDefID: uuid.New().String(), OrgID: orgID, BoardID: boardID,
		Kind: FieldKindStatus, Value: "victim-" + uuid.New().String(), Label: "Victim",
		Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := f.Add(context.Background()); err != nil {
		t.Fatalf("seed field def: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM ticket_field_defs WHERE field_def_id = ?`, f.FieldDefID) }) //nolint:errcheck
	return f
}

func TestNonOwnerIsDeniedOnEveryPerRecordRoute(t *testing.T) {
	requireDB(t)

	const owner, stranger = "u-owner", "u-stranger"
	tk := seedTicketFor(t, owner, "org-owner", "ownername")
	comment := seedCommentOn(t, tk.TicketID, owner)
	board := seedBoardFor(t, owner, "org-owner")
	boardDef := seedFieldDefOn(t, board.BoardID, "org-owner")

	// Authorized, as a different user with no org. The empty orgID also pins that
	// tenancy cannot match by both sides being blank.
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"`+stranger+`"}`)
	// The raw forward-the-bearer helpers (project/platform checks) get their own stub,
	// answering NO — a stranger holds no project role over the victim's board.
	stubGatekeeperHTTP(t, false)

	ticketBody := `{"title":"hijacked"}`

	routes := []struct {
		name   string
		method string
		target string
		vals   map[string]string
		body   string
		h      http.HandlerFunc
	}{
		{"GET /tickets/{id}", http.MethodGet, "/tickets/" + tk.TicketID,
			map[string]string{"id": tk.TicketID}, "", handleGetTicket},
		{"PUT /tickets/{id}", http.MethodPut, "/tickets/" + tk.TicketID,
			map[string]string{"id": tk.TicketID}, ticketBody, handleUpdateTicket},
		{"DELETE /tickets/{id}", http.MethodDelete, "/tickets/" + tk.TicketID,
			map[string]string{"id": tk.TicketID}, "", handleDeleteTicket},

		// The owner-first routes, addressed with the OWNER's real namespace — the
		// strongest form of the attack, since the caller names the true owner rather
		// than guessing.
		{"GET /tickets/{ns}/{id}", http.MethodGet, "/tickets/ownername/" + tk.TicketID,
			map[string]string{"ns": "ownername", "id": tk.TicketID}, "", handleGetTicket},
		{"PUT /tickets/{ns}/{id}", http.MethodPut, "/tickets/ownername/" + tk.TicketID,
			map[string]string{"ns": "ownername", "id": tk.TicketID}, ticketBody, handleUpdateTicket},
		{"DELETE /tickets/{ns}/{id}", http.MethodDelete, "/tickets/ownername/" + tk.TicketID,
			map[string]string{"ns": "ownername", "id": tk.TicketID}, "", handleDeleteTicket},

		// ...and with the STRANGER's own namespace, which is what a caller would try in
		// order to make their own grants match someone else's id.
		{"GET /tickets/{ns}/{id} as own ns", http.MethodGet, "/tickets/strangername/" + tk.TicketID,
			map[string]string{"ns": "strangername", "id": tk.TicketID}, "", handleGetTicket},
		{"DELETE /tickets/{ns}/{id} as own ns", http.MethodDelete, "/tickets/strangername/" + tk.TicketID,
			map[string]string{"ns": "strangername", "id": tk.TicketID}, "", handleDeleteTicket},

		{"POST /tickets/{id}/comments", http.MethodPost, "/tickets/" + tk.TicketID + "/comments",
			map[string]string{"id": tk.TicketID}, `{"body":"hijacked"}`, handleAddComment},
		{"DELETE /tickets/{id}/comments/{comment_id}", http.MethodDelete,
			"/tickets/" + tk.TicketID + "/comments/" + comment.CommentID,
			map[string]string{"id": tk.TicketID, "comment_id": comment.CommentID}, "", handleDeleteComment},
		{"POST /tickets/{ns}/{id}/comments", http.MethodPost,
			"/tickets/ownername/" + tk.TicketID + "/comments",
			map[string]string{"ns": "ownername", "id": tk.TicketID}, `{"body":"hijacked"}`, handleAddComment},
		{"DELETE /tickets/{ns}/{id}/comments/{comment_id}", http.MethodDelete,
			"/tickets/ownername/" + tk.TicketID + "/comments/" + comment.CommentID,
			map[string]string{"ns": "ownername", "id": tk.TicketID, "comment_id": comment.CommentID}, "", handleDeleteComment},

		{"GET /boards/{id}", http.MethodGet, "/boards/" + board.BoardID,
			map[string]string{"id": board.BoardID}, "", handleGetBoard},
		{"PUT /boards/{id}", http.MethodPut, "/boards/" + board.BoardID,
			map[string]string{"id": board.BoardID}, `{"name":"hijacked"}`, handleUpdateBoard},
		{"DELETE /boards/{id}", http.MethodDelete, "/boards/" + board.BoardID,
			map[string]string{"id": board.BoardID}, "", handleDeleteBoard},

		{"PUT /field-defs/{id}", http.MethodPut, "/field-defs/" + boardDef.FieldDefID,
			map[string]string{"id": boardDef.FieldDefID}, `{"label":"hijacked"}`, handleUpdateFieldDef},
		{"DELETE /field-defs/{id}", http.MethodDelete, "/field-defs/" + boardDef.FieldDefID,
			map[string]string{"id": boardDef.FieldDefID}, "", handleDeleteFieldDef},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			r := httptest.NewRequest(rt.method, rt.target, strings.NewReader(rt.body))
			r.Header.Set("Authorization", "Bearer stranger-tok")
			r.Header.Set("Content-Type", "application/json")
			for k, v := range rt.vals {
				r.SetPathValue(k, v)
			}
			w := httptest.NewRecorder()
			rt.h(w, r)

			// 404 is preferred (it does not confirm the id exists); 403 is acceptable.
			// Anything else, above all a 2xx, is the leak.
			if w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
				t.Errorf("a non-owner got %d, want 404 or 403: %s", w.Code, w.Body.String())
			}
			for _, secret := range []string{tk.Title, board.Name, comment.Body} {
				if strings.Contains(w.Body.String(), secret) {
					t.Errorf("denial body leaked %q: %s", secret, w.Body.String())
				}
			}
		})
	}

	// A route that answered 404 while still performing the write would pass every
	// assertion above, so check the rows themselves.
	t.Run("victim rows untouched", func(t *testing.T) {
		got, err := getTicket(context.Background(), tk.TicketID)
		if err != nil {
			t.Fatalf("ticket was deleted or is unreadable: %v", err)
		}
		if got.Title != tk.Title || got.CreatedBy != owner {
			t.Errorf("ticket mutated: title=%q created_by=%q", got.Title, got.CreatedBy)
		}

		gotBoard, err := getBoard(context.Background(), board.BoardID)
		if err != nil {
			t.Fatalf("board was deleted or is unreadable: %v", err)
		}
		if gotBoard.Name != board.Name {
			t.Errorf("board mutated: name=%q", gotBoard.Name)
		}

		row, err := (TicketFieldDef{FieldDefID: boardDef.FieldDefID}).Get(context.Background())
		if err != nil {
			t.Fatalf("field def was deleted or is unreadable: %v", err)
		}
		if f := row.(TicketFieldDef); f.Label != boardDef.Label {
			t.Errorf("field def mutated: label=%q", f.Label)
		}

		comments, err := listComments(context.Background(), tk.TicketID)
		if err != nil {
			t.Fatalf("list comments: %v", err)
		}
		if len(comments) != 1 {
			t.Errorf("comment count = %d, want 1 — a stranger added or removed one", len(comments))
		}
	})
}
