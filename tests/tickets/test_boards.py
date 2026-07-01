"""Integration tests for ticket board CRUD and ticket↔board assignment."""
import uuid

import requests

from conftest import TICKETS_URL


# ── Authentication ─────────────────────────────────────────────────────────────

def test_board_create_unauthorized():
    res = requests.post(f"{TICKETS_URL}/boards", json={"name": "x"})
    assert res.status_code == 401

def test_board_list_unauthorized():
    res = requests.get(f"{TICKETS_URL}/boards")
    assert res.status_code == 401

def test_board_delete_unauthorized():
    res = requests.delete(f"{TICKETS_URL}/boards/does-not-matter")
    assert res.status_code == 401


# ── Create / validation ────────────────────────────────────────────────────────

def test_board_create_missing_name(bearer):
    res = requests.post(f"{TICKETS_URL}/boards", headers=bearer, json={"description": "no name"})
    assert res.status_code == 400

def test_board_create_returns_201(bearer):
    name = f"Sprint {uuid.uuid4().hex[:8]}"
    res = requests.post(f"{TICKETS_URL}/boards", headers=bearer,
                        json={"name": name, "description": "Q3 work", "color": "#aabbcc"})
    assert res.status_code == 201, res.text
    b = res.json()
    assert b["name"] == name
    assert b["description"] == "Q3 work"
    assert b["board_id"]

def test_board_create_duplicate_name_conflicts(bearer):
    name = f"Dup {uuid.uuid4().hex[:8]}"
    assert requests.post(f"{TICKETS_URL}/boards", headers=bearer, json={"name": name}).status_code == 201
    res = requests.post(f"{TICKETS_URL}/boards", headers=bearer, json={"name": name})
    assert res.status_code == 409


# ── List / Get ─────────────────────────────────────────────────────────────────

def test_board_list_includes_created(bearer):
    name = f"Listed {uuid.uuid4().hex[:8]}"
    requests.post(f"{TICKETS_URL}/boards", headers=bearer, json={"name": name})
    res = requests.get(f"{TICKETS_URL}/boards", headers=bearer)
    assert res.status_code == 200
    assert name in [b["name"] for b in res.json()]

def test_board_get_other_user_not_visible(bearer, other_bearer):
    res = requests.post(f"{TICKETS_URL}/boards", headers=bearer, json={"name": f"Private {uuid.uuid4().hex[:8]}"})
    bid = res.json()["board_id"]
    assert requests.get(f"{TICKETS_URL}/boards/{bid}", headers=other_bearer).status_code == 404


# ── Update ─────────────────────────────────────────────────────────────────────

def test_board_update_name_and_color(bearer):
    res = requests.post(f"{TICKETS_URL}/boards", headers=bearer, json={"name": f"Orig {uuid.uuid4().hex[:8]}"})
    bid = res.json()["board_id"]
    new_name = f"Renamed {uuid.uuid4().hex[:8]}"
    res = requests.put(f"{TICKETS_URL}/boards/{bid}", headers=bearer,
                       json={"name": new_name, "color": "#123456"})
    assert res.status_code == 200, res.text
    assert res.json()["name"] == new_name
    assert res.json()["color"] == "#123456"


# ── Ticket ↔ board assignment ──────────────────────────────────────────────────

def test_ticket_create_with_board(bearer):
    res = requests.post(f"{TICKETS_URL}/boards", headers=bearer, json={"name": f"Board {uuid.uuid4().hex[:8]}"})
    bid = res.json()["board_id"]
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "On a board", "board_id": bid})
    assert res.status_code == 201, res.text
    assert res.json()["board_id"] == bid

def test_ticket_create_with_unknown_board_rejected(bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "Bad board", "board_id": "00000000-0000-0000-0000-000000000000"})
    assert res.status_code == 400

def test_ticket_create_without_board_gets_default(bearer):
    # Every ticket has a board: omitting board_id lands the ticket on the caller's
    # auto-created "Default" board rather than leaving it unassigned.
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "No board given"})
    assert res.status_code == 201, res.text
    bid = res.json().get("board_id")
    assert bid, "ticket created without a board should be placed on the default board"
    b = requests.get(f"{TICKETS_URL}/boards/{bid}", headers=bearer)
    assert b.status_code == 200
    assert b.json()["name"] == "Default"

def test_ticket_list_filter_by_board(bearer):
    res = requests.post(f"{TICKETS_URL}/boards", headers=bearer, json={"name": f"Filter {uuid.uuid4().hex[:8]}"})
    bid = res.json()["board_id"]
    requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "Filterable", "board_id": bid})
    res = requests.get(f"{TICKETS_URL}/tickets?board_id={bid}", headers=bearer)
    assert res.status_code == 200
    tickets = res.json()
    assert len(tickets) >= 1
    assert all(t["board_id"] == bid for t in tickets)

def test_ticket_update_empty_board_rehomes_to_default(bearer):
    res = requests.post(f"{TICKETS_URL}/boards", headers=bearer, json={"name": f"Clearable {uuid.uuid4().hex[:8]}"})
    bid = res.json()["board_id"]
    tid = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "Movable", "board_id": bid}).json()["ticket_id"]
    # Clearing the board ("") re-homes the ticket to the caller's default board
    # rather than leaving it board-less — every ticket must have a board.
    res = requests.put(f"{TICKETS_URL}/tickets/{tid}", headers=bearer, json={"board_id": ""})
    assert res.status_code == 200, res.text
    new_board = res.json().get("board_id")
    assert new_board, "ticket should still have a board after clearing"
    assert new_board != bid


# ── Delete requires name confirmation and cascade-deletes tickets ──────────────

def test_board_delete_requires_name_confirmation_and_cascades(bearer):
    res = requests.post(f"{TICKETS_URL}/boards", headers=bearer, json={"name": f"Doomed {uuid.uuid4().hex[:8]}"})
    board = res.json()
    bid, name = board["board_id"], board["name"]
    tid = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "Casualty", "board_id": bid}).json()["ticket_id"]

    # No confirmation → rejected; board and ticket untouched.
    assert requests.delete(f"{TICKETS_URL}/boards/{bid}", headers=bearer).status_code == 400
    # Wrong name → rejected.
    assert requests.delete(f"{TICKETS_URL}/boards/{bid}", headers=bearer,
                           params={"confirm": "not the name"}).status_code == 400
    assert requests.get(f"{TICKETS_URL}/boards/{bid}", headers=bearer).status_code == 200

    # Correct name → board deleted and its tickets cascade-deleted with it.
    assert requests.delete(f"{TICKETS_URL}/boards/{bid}", headers=bearer,
                           params={"confirm": name}).status_code == 204
    assert requests.get(f"{TICKETS_URL}/boards/{bid}", headers=bearer).status_code == 404
    assert requests.get(f"{TICKETS_URL}/tickets/{tid}", headers=bearer).status_code == 404
