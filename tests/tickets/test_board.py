"""Integration tests for the API flows used by the tickets board TUI.

The board makes two kinds of requests:
  GET /tickets/tickets         - populate all four columns from one unfiltered fetch
  PUT /tickets/tickets/{id}    - move a card (sends only {"title": ..., "status": ...})

Tests here focus on the patterns the board relies on that are not fully
covered by test_tickets.py.
"""
import pytest
import requests

from conftest import TICKETS_URL


# ── helpers ───────────────────────────────────────────────────────────────────

def create_ticket(bearer, title="Board test", priority="medium", **kwargs):
    payload = {"title": title, "priority": priority, **kwargs}
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json=payload)
    assert res.status_code == 201, res.text
    return res.json()


def move_ticket(bearer, ticket_id, title, new_status):
    """Simulate the exact PUT the board sends: only title and status."""
    res = requests.put(
        f"{TICKETS_URL}/tickets/{ticket_id}",
        headers=bearer,
        json={"title": title, "status": new_status},
    )
    return res


# ── List response shape ───────────────────────────────────────────────────────

def test_board_list_has_required_fields(bearer):
    """GET /tickets returns objects with all fields the boardTicket struct needs."""
    create_ticket(bearer, "Field check ticket")
    res = requests.get(f"{TICKETS_URL}/tickets", headers=bearer)
    assert res.status_code == 200
    tickets = res.json()
    assert len(tickets) >= 1
    for t in tickets:
        assert "ticket_id" in t, "missing ticket_id"
        assert "title" in t, "missing title"
        assert "priority" in t, "missing priority"
        assert "status" in t, "missing status"


def test_board_list_unfiltered_returns_all_statuses(bearer):
    """Board fetches with no query params; tickets in every status must appear."""
    t1 = create_ticket(bearer, "Board open ticket")
    t2 = create_ticket(bearer, "Board in-progress ticket")
    t3 = create_ticket(bearer, "Board resolved ticket")
    t4 = create_ticket(bearer, "Board closed ticket")

    # Move each ticket to its target status.
    move_ticket(bearer, t2["ticket_id"], t2["title"], "in_progress")
    move_ticket(bearer, t3["ticket_id"], t3["title"], "resolved")
    move_ticket(bearer, t4["ticket_id"], t4["title"], "closed")

    # Fetch without any filter — board always does this.
    res = requests.get(f"{TICKETS_URL}/tickets", headers=bearer)
    assert res.status_code == 200
    tickets = res.json()
    ids = {t["ticket_id"] for t in tickets}
    assert t1["ticket_id"] in ids, "open ticket missing from unfiltered list"
    assert t2["ticket_id"] in ids, "in_progress ticket missing from unfiltered list"
    assert t3["ticket_id"] in ids, "resolved ticket missing from unfiltered list"
    assert t4["ticket_id"] in ids, "closed ticket missing from unfiltered list"

    # Verify each appears with the correct status.
    by_id = {t["ticket_id"]: t for t in tickets}
    assert by_id[t1["ticket_id"]]["status"] == "open"
    assert by_id[t2["ticket_id"]]["status"] == "in_progress"
    assert by_id[t3["ticket_id"]]["status"] == "resolved"
    assert by_id[t4["ticket_id"]]["status"] == "closed"


# ── Minimal PUT (board's move operation) ──────────────────────────────────────

def test_board_move_open_to_in_progress(bearer):
    """L key: open → in_progress — the first column move not tested elsewhere."""
    t = create_ticket(bearer, "Move me right")
    res = move_ticket(bearer, t["ticket_id"], t["title"], "in_progress")
    assert res.status_code == 200, res.text
    assert res.json()["status"] == "in_progress"


def test_board_minimal_put_preserves_priority(bearer):
    """Board sends only title+status; existing priority must not be reset."""
    t = create_ticket(bearer, "Priority preserved", priority="critical")
    assert t["priority"] == "critical"

    res = move_ticket(bearer, t["ticket_id"], t["title"], "in_progress")
    assert res.status_code == 200, res.text
    updated = res.json()
    assert updated["status"] == "in_progress"
    assert updated["priority"] == "critical", (
        f"priority changed to {updated['priority']!r} after board move"
    )


def test_board_minimal_put_preserves_description(bearer):
    """Board sends only title+status; existing description must not be cleared."""
    t = create_ticket(bearer, "Desc preserved", description="Keep me around")
    res = move_ticket(bearer, t["ticket_id"], t["title"], "in_progress")
    assert res.status_code == 200, res.text
    full = requests.get(f"{TICKETS_URL}/tickets/{t['ticket_id']}", headers=bearer).json()
    assert full.get("description") == "Keep me around", (
        f"description was cleared: {full.get('description')!r}"
    )


# ── Full kanban forward lifecycle ─────────────────────────────────────────────

def test_board_full_forward_lifecycle(bearer):
    """L key path: open → in_progress → resolved → closed, one move at a time."""
    t = create_ticket(bearer, "Full lifecycle")
    tid, title = t["ticket_id"], t["title"]

    transitions = ["in_progress", "resolved", "closed"]
    for status in transitions:
        res = move_ticket(bearer, tid, title, status)
        assert res.status_code == 200, f"move to {status!r} failed: {res.text}"
        assert res.json()["status"] == status


# ── Backward moves (board's H key) ────────────────────────────────────────────

def test_board_backward_move_in_progress_to_open(bearer):
    """H key: in_progress → open (moving left back to the first column)."""
    t = create_ticket(bearer, "Move backward")
    tid, title = t["ticket_id"], t["title"]

    move_ticket(bearer, tid, title, "in_progress")
    res = move_ticket(bearer, tid, title, "open")
    assert res.status_code == 200, res.text
    assert res.json()["status"] == "open"


def test_board_backward_move_resolved_to_in_progress(bearer):
    """H key: resolved → in_progress."""
    t = create_ticket(bearer, "Backward resolved")
    tid, title = t["ticket_id"], t["title"]

    move_ticket(bearer, tid, title, "resolved")
    res = move_ticket(bearer, tid, title, "in_progress")
    assert res.status_code == 200, res.text
    assert res.json()["status"] == "in_progress"


def test_board_backward_move_closed_to_resolved(bearer):
    """H key: closed → resolved (leftmost backward move the board can make)."""
    t = create_ticket(bearer, "Reopen from closed")
    tid, title = t["ticket_id"], t["title"]

    move_ticket(bearer, tid, title, "closed")
    res = move_ticket(bearer, tid, title, "resolved")
    assert res.status_code == 200, res.text
    assert res.json()["status"] == "resolved"


# ── Board column population ───────────────────────────────────────────────────

def test_board_list_status_matches_column_bucket(bearer):
    """Each ticket in the list has a status that maps to one of the 4 board columns."""
    valid_statuses = {"open", "in_progress", "resolved", "closed"}
    create_ticket(bearer, "Column bucket check")
    res = requests.get(f"{TICKETS_URL}/tickets", headers=bearer)
    assert res.status_code == 200
    for t in res.json():
        assert t["status"] in valid_statuses, (
            f"ticket {t['ticket_id']!r} has unexpected status {t['status']!r}"
        )
