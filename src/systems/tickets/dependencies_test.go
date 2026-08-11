package main

import (
	"context"
	"testing"
	"time"
)

// addTestTicket inserts a ticket owned by the given user.
func addTestTicket(t *testing.T, id, owner, status string) Ticket {
	t.Helper()
	tk := Ticket{
		TicketID:  id,
		Title:     "ticket " + id,
		Status:    status,
		CreatedBy: owner,
		Active:    true,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := tk.Add(context.Background()); err != nil {
		t.Fatalf("add ticket %s: %v", id, err)
	}
	t.Cleanup(func() {
		_ = removeDependenciesOf(context.Background(), id)
		_ = tk.Remove(context.Background())
	})
	return tk
}

func addTestDependency(t *testing.T, ticketID, dependsOn string) {
	t.Helper()
	d := TicketDependency{
		TicketID:    ticketID,
		DependsOnID: dependsOn,
		CreatedBy:   "alice",
		CreatedAt:   time.Now().UTC(),
	}
	if err := d.Add(context.Background()); err != nil {
		t.Fatalf("add dependency %s->%s: %v", ticketID, dependsOn, err)
	}
}

// A cycle is not cosmetic: every ticket in it waits for another ticket in it, so
// the whole set is unworkable forever — and an agent runtime polling for ready
// work would simply stop seeing them, with nothing to say why.
func TestDependencyCycleIsRefused(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	addTestTicket(t, "cyc-a", "alice", StatusOpen)
	addTestTicket(t, "cyc-b", "alice", StatusOpen)
	addTestTicket(t, "cyc-c", "alice", StatusOpen)

	// a → b → c
	addTestDependency(t, "cyc-a", "cyc-b")
	addTestDependency(t, "cyc-b", "cyc-c")

	// c depending on a would close the loop.
	cyclic, err := wouldCycle(ctx, "cyc-c", "cyc-a")
	if err != nil {
		t.Fatalf("wouldCycle: %v", err)
	}
	if !cyclic {
		t.Error("a transitive cycle was not detected; the whole chain would be unworkable forever")
	}

	// Something outside the chain is fine.
	addTestTicket(t, "cyc-d", "alice", StatusOpen)
	cyclic, err = wouldCycle(ctx, "cyc-d", "cyc-a")
	if err != nil {
		t.Fatalf("wouldCycle: %v", err)
	}
	if cyclic {
		t.Error("refused an acyclic dependency")
	}
}

// Declaring the same dependency twice must be idempotent: the caller's intent is
// already true, and an error would force every client to read before writing.
func TestDependencyAddIsIdempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	addTestTicket(t, "idem-a", "alice", StatusOpen)
	addTestTicket(t, "idem-b", "alice", StatusOpen)

	addTestDependency(t, "idem-a", "idem-b")
	addTestDependency(t, "idem-a", "idem-b")

	ids, err := dependencyIDs(ctx, "idem-a")
	if err != nil {
		t.Fatalf("dependencyIDs: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("dependencies = %v, want exactly one row", ids)
	}
}

// THE LISTING IS THE POINT. Comments are deliberately blanked on listings, and a
// consumer that judged readiness from a listing was silently wrong for a day
// because of it. Dependencies must not repeat that: deciding which tickets can
// be worked now is the main reason to ask for a column, and answering it
// per-ticket would be an N+1.
func TestDependenciesArePresentOnListings(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	addTestTicket(t, "list-blocked", "alice", StatusOpen)
	blocker := addTestTicket(t, "list-blocker", "alice", StatusInProgress)
	addTestTicket(t, "list-free", "alice", StatusOpen)
	addTestDependency(t, "list-blocked", "list-blocker")

	tickets := []Ticket{
		{TicketID: "list-blocked", CreatedBy: "alice"},
		{TicketID: "list-free", CreatedBy: "alice"},
	}
	if err := loadDependencies(ctx, tickets, "alice", ""); err != nil {
		t.Fatalf("loadDependencies: %v", err)
	}

	if len(tickets[0].DependsOn) != 1 {
		t.Fatalf("blocked ticket has %d dependencies, want 1", len(tickets[0].DependsOn))
	}
	got := tickets[0].DependsOn[0]
	if got.TicketID != "list-blocker" {
		t.Errorf("depends_on = %q, want list-blocker", got.TicketID)
	}
	// The STATUS must come back, or the consumer has to fetch every blocker to
	// find out whether the block still applies — the N+1 this exists to avoid.
	if got.Status != blocker.Status {
		t.Errorf("depends_on status = %q, want %q", got.Status, blocker.Status)
	}
	if got.Title == "" {
		t.Error("depends_on carries no title, so a person cannot see what blocks the ticket")
	}

	// An unblocked ticket must say so explicitly, not with a nil that a client
	// cannot distinguish from "not loaded".
	if tickets[1].DependsOn == nil {
		t.Error("an unblocked ticket has nil depends_on; it must be an empty list")
	}
	if len(tickets[1].DependsOn) != 0 {
		t.Errorf("unblocked ticket has dependencies: %v", tickets[1].DependsOn)
	}
}

// A blocker the caller cannot see is reported as a bare id. They must know the
// work is blocked; they must not learn what by.
func TestDependencyOnAnInaccessibleTicketDoesNotLeak(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	addTestTicket(t, "leak-mine", "alice", StatusOpen)
	addTestTicket(t, "leak-theirs", "bob", StatusInProgress)
	addTestDependency(t, "leak-mine", "leak-theirs")

	tickets := []Ticket{{TicketID: "leak-mine", CreatedBy: "alice"}}
	if err := loadDependencies(ctx, tickets, "alice", ""); err != nil {
		t.Fatalf("loadDependencies: %v", err)
	}
	if len(tickets[0].DependsOn) != 1 {
		t.Fatalf("dependencies = %v, want the block to remain visible", tickets[0].DependsOn)
	}
	got := tickets[0].DependsOn[0]
	if got.TicketID != "leak-theirs" {
		t.Errorf("depends_on = %q, want the id to still be reported", got.TicketID)
	}
	if got.Title != "" || got.Status != "" {
		t.Errorf("leaked another user's ticket: title=%q status=%q", got.Title, got.Status)
	}
}

// Deleting a blocker must not leave tickets waiting on something that no longer
// exists — that is permanently unworkable with nothing left to explain it.
func TestDeletingATicketClearsItsDependencyEdges(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	addTestTicket(t, "del-blocked", "alice", StatusOpen)
	addTestTicket(t, "del-blocker", "alice", StatusOpen)
	addTestDependency(t, "del-blocked", "del-blocker")

	if err := removeDependenciesOf(ctx, "del-blocker"); err != nil {
		t.Fatalf("removeDependenciesOf: %v", err)
	}
	ids, err := dependencyIDs(ctx, "del-blocked")
	if err != nil {
		t.Fatalf("dependencyIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("ticket still blocked by a deleted ticket: %v", ids)
	}
}
