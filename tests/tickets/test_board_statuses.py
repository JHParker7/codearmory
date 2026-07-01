"""Integration tests for board-scoped status columns and the left-most default.

A board owns its own status columns (seeded on creation); a new ticket defaults
to that board's left-most column, and statuses are validated per board.
"""
import uuid

import requests

from conftest import TICKETS_URL


def _make_board(bearer, name=None):
    name = name or f"Board {uuid.uuid4().hex[:8]}"
    res = requests.post(f"{TICKETS_URL}/boards", headers=bearer, json={"name": name})
    assert res.status_code == 201, res.text
    return res.json()["board_id"]


def _board_statuses(bearer, board_id):
    res = requests.get(f"{TICKETS_URL}/field-defs?kind=status&board_id={board_id}", headers=bearer)
    assert res.status_code == 200, res.text
    return sorted(res.json(), key=lambda d: d["position"])


# ── Seeding ─────────────────────────────────────────────────────────────────────

def test_new_board_seeds_its_own_status_columns(bearer):
    bid = _make_board(bearer)
    cols = _board_statuses(bearer, bid)
    assert len(cols) >= 1
    # The seeded columns are owned by this board, not the org/global set.
    assert all(d.get("board_id") == bid for d in cols)


def test_board_columns_are_independent(bearer):
    a, b = _make_board(bearer), _make_board(bearer)
    label = f"Triage {uuid.uuid4().hex[:6]}"
    value = f"triage_{uuid.uuid4().hex[:6]}"
    res = requests.post(f"{TICKETS_URL}/field-defs", headers=bearer,
                        json={"kind": "status", "value": value, "label": label, "board_id": a, "position": 9})
    assert res.status_code == 201, res.text

    a_values = [d["value"] for d in _board_statuses(bearer, a)]
    b_values = [d["value"] for d in _board_statuses(bearer, b)]
    assert value in a_values
    assert value not in b_values


def test_two_boards_may_share_a_status_value(bearer):
    # The same value on two different boards must not collide on the unique index.
    a, b = _make_board(bearer), _make_board(bearer)
    value = f"shared_{uuid.uuid4().hex[:6]}"
    for board in (a, b):
        res = requests.post(f"{TICKETS_URL}/field-defs", headers=bearer,
                            json={"kind": "status", "value": value, "label": "Shared", "board_id": board})
        assert res.status_code == 201, res.text


# ── Validation ──────────────────────────────────────────────────────────────────

def test_board_scoped_def_must_be_status(bearer):
    bid = _make_board(bearer)
    res = requests.post(f"{TICKETS_URL}/field-defs", headers=bearer,
                        json={"kind": "priority", "value": "p9", "label": "P9", "board_id": bid})
    assert res.status_code == 400, res.text


def test_board_scoped_def_unknown_board_rejected(bearer):
    res = requests.post(f"{TICKETS_URL}/field-defs", headers=bearer,
                        json={"kind": "status", "value": "x", "label": "X",
                              "board_id": "00000000-0000-0000-0000-000000000000"})
    assert res.status_code == 400, res.text


# ── Left-most default ───────────────────────────────────────────────────────────

def test_new_ticket_defaults_to_leftmost_column(bearer):
    bid = _make_board(bearer)
    leftmost = _board_statuses(bearer, bid)[0]["value"]
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "Default status", "board_id": bid})
    assert res.status_code == 201, res.text
    assert res.json()["status"] == leftmost


def test_new_ticket_accepts_explicit_board_status(bearer):
    bid = _make_board(bearer)
    cols = _board_statuses(bearer, bid)
    # Pick a non-left-most column when the board has more than one.
    chosen = cols[-1]["value"]
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "Explicit status", "board_id": bid, "status": chosen})
    assert res.status_code == 201, res.text
    assert res.json()["status"] == chosen


def test_new_ticket_rejects_status_not_on_board(bearer):
    bid = _make_board(bearer)
    # Add a column to a *different* board and confirm it isn't valid here.
    other = _make_board(bearer)
    value = f"only_other_{uuid.uuid4().hex[:6]}"
    requests.post(f"{TICKETS_URL}/field-defs", headers=bearer,
                  json={"kind": "status", "value": value, "label": "Only Other", "board_id": other})
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "Wrong status", "board_id": bid, "status": value})
    assert res.status_code == 400, res.text
