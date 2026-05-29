"""Integration tests for ticket CRUD endpoints."""
import pytest
import requests

from conftest import TICKETS_URL


# ── Authentication ─────────────────────────────────────────────────────────────

def test_create_unauthorized():
    res = requests.post(f"{TICKETS_URL}/tickets", json={"title": "x"})
    assert res.status_code == 401

def test_list_unauthorized():
    res = requests.get(f"{TICKETS_URL}/tickets")
    assert res.status_code == 401

def test_get_unauthorized():
    res = requests.get(f"{TICKETS_URL}/tickets/does-not-matter")
    assert res.status_code == 401

def test_update_unauthorized():
    res = requests.put(f"{TICKETS_URL}/tickets/does-not-matter", json={"title": "x"})
    assert res.status_code == 401

def test_delete_unauthorized():
    res = requests.delete(f"{TICKETS_URL}/tickets/does-not-matter")
    assert res.status_code == 401


# ── Create ─────────────────────────────────────────────────────────────────────

def test_create_missing_title(bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"description": "no title"})
    assert res.status_code == 400

def test_create_invalid_priority(bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "x", "priority": "urgent"})
    assert res.status_code == 400

def test_create_returns_201(bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "Deploy v1.2", "description": "Push to prod"})
    assert res.status_code == 201, res.text
    t = res.json()
    assert t["ticket_id"] != ""
    assert t["title"] == "Deploy v1.2"
    assert t["status"] == "open"
    assert t["priority"] == "medium"
    assert t["comments"] == []

def test_create_with_priority(bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "Critical bug", "priority": "critical"})
    assert res.status_code == 201
    assert res.json()["priority"] == "critical"

def test_create_with_linked_resources(bearer):
    wf_id = "wf-00000000-0000-0000-0000-000000000001"
    fe_id = "fe-00000000-0000-0000-0000-000000000002"
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={
        "title": "Linked ticket",
        "workflow_id": wf_id,
        "forge_execution_id": fe_id,
    })
    assert res.status_code == 201
    t = res.json()
    assert t["workflow_id"] == wf_id
    assert t["forge_execution_id"] == fe_id


# ── List ───────────────────────────────────────────────────────────────────────

def test_list_returns_array(bearer):
    res = requests.get(f"{TICKETS_URL}/tickets", headers=bearer)
    assert res.status_code == 200
    assert isinstance(res.json(), list)

def test_list_includes_own_tickets(bearer):
    requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "Findable"})
    res = requests.get(f"{TICKETS_URL}/tickets", headers=bearer)
    assert res.status_code == 200
    titles = [t["title"] for t in res.json()]
    assert "Findable" in titles

def test_list_filter_by_status(bearer):
    requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "Status filter test"})
    res = requests.get(f"{TICKETS_URL}/tickets?status=open", headers=bearer)
    assert res.status_code == 200
    for t in res.json():
        assert t["status"] == "open"

def test_list_filter_by_priority(bearer):
    requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                  json={"title": "Prio filter", "priority": "high"})
    res = requests.get(f"{TICKETS_URL}/tickets?priority=high", headers=bearer)
    assert res.status_code == 200
    for t in res.json():
        assert t["priority"] == "high"

def test_list_invalid_status_filter(bearer):
    res = requests.get(f"{TICKETS_URL}/tickets?status=bogus", headers=bearer)
    assert res.status_code == 400

def test_list_invalid_priority_filter(bearer):
    res = requests.get(f"{TICKETS_URL}/tickets?priority=extreme", headers=bearer)
    assert res.status_code == 400


# ── Get ────────────────────────────────────────────────────────────────────────

def test_get_not_found(bearer):
    res = requests.get(f"{TICKETS_URL}/tickets/00000000-0000-0000-0000-000000000000",
                       headers=bearer)
    assert res.status_code == 404

def test_get_own_ticket(bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "Get me"})
    tid = res.json()["ticket_id"]

    res = requests.get(f"{TICKETS_URL}/tickets/{tid}", headers=bearer)
    assert res.status_code == 200
    t = res.json()
    assert t["ticket_id"] == tid
    assert t["title"] == "Get me"
    assert isinstance(t["comments"], list)

def test_get_other_user_ticket_not_visible(bearer, other_bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "Private"})
    tid = res.json()["ticket_id"]

    res = requests.get(f"{TICKETS_URL}/tickets/{tid}", headers=other_bearer)
    assert res.status_code == 404


# ── Update ─────────────────────────────────────────────────────────────────────

def test_update_not_found(bearer):
    res = requests.put(f"{TICKETS_URL}/tickets/00000000-0000-0000-0000-000000000000",
                       headers=bearer, json={"title": "x"})
    assert res.status_code == 404

def test_update_own_ticket(bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "Update me"})
    tid = res.json()["ticket_id"]

    res = requests.put(f"{TICKETS_URL}/tickets/{tid}", headers=bearer, json={
        "title": "Updated title",
        "status": "in_progress",
        "priority": "high",
    })
    assert res.status_code == 200
    t = res.json()
    assert t["title"] == "Updated title"
    assert t["status"] == "in_progress"
    assert t["priority"] == "high"

def test_update_invalid_status(bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "T"})
    tid = res.json()["ticket_id"]
    res = requests.put(f"{TICKETS_URL}/tickets/{tid}", headers=bearer,
                       json={"title": "T", "status": "invalid"})
    assert res.status_code == 400

def test_update_other_user_ticket_not_visible(bearer, other_bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "Protected"})
    tid = res.json()["ticket_id"]
    res = requests.put(f"{TICKETS_URL}/tickets/{tid}", headers=other_bearer,
                       json={"title": "Hijacked"})
    assert res.status_code == 404

def test_update_to_resolved_and_closed(bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "Lifecycle"})
    tid = res.json()["ticket_id"]

    res = requests.put(f"{TICKETS_URL}/tickets/{tid}", headers=bearer,
                       json={"title": "Lifecycle", "status": "resolved"})
    assert res.status_code == 200
    assert res.json()["status"] == "resolved"

    res = requests.put(f"{TICKETS_URL}/tickets/{tid}", headers=bearer,
                       json={"title": "Lifecycle", "status": "closed"})
    assert res.status_code == 200
    assert res.json()["status"] == "closed"


# ── Delete ─────────────────────────────────────────────────────────────────────

def test_delete_not_found(bearer):
    res = requests.delete(f"{TICKETS_URL}/tickets/00000000-0000-0000-0000-000000000000",
                          headers=bearer)
    assert res.status_code == 404

def test_delete_own_ticket(bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "Delete me"})
    tid = res.json()["ticket_id"]

    res = requests.delete(f"{TICKETS_URL}/tickets/{tid}", headers=bearer)
    assert res.status_code == 204

    res = requests.get(f"{TICKETS_URL}/tickets/{tid}", headers=bearer)
    assert res.status_code == 404

def test_delete_other_user_ticket_not_visible(bearer, other_bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer, json={"title": "Protected 2"})
    tid = res.json()["ticket_id"]
    res = requests.delete(f"{TICKETS_URL}/tickets/{tid}", headers=other_bearer)
    assert res.status_code == 404
