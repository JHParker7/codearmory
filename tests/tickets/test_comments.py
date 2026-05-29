"""Integration tests for ticket comment endpoints."""
import pytest
import requests

from conftest import TICKETS_URL


@pytest.fixture
def ticket(bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "Comment test ticket"})
    assert res.status_code == 201
    t = res.json()
    yield t
    requests.delete(f"{TICKETS_URL}/tickets/{t['ticket_id']}", headers=bearer)


# ── Authentication ─────────────────────────────────────────────────────────────

def test_add_comment_unauthorized(ticket):
    res = requests.post(f"{TICKETS_URL}/tickets/{ticket['ticket_id']}/comments",
                        json={"body": "hello"})
    assert res.status_code == 401

def test_delete_comment_unauthorized(ticket):
    res = requests.delete(
        f"{TICKETS_URL}/tickets/{ticket['ticket_id']}/comments/does-not-matter"
    )
    assert res.status_code == 401


# ── Add comment ────────────────────────────────────────────────────────────────

def test_add_comment_missing_body(bearer, ticket):
    res = requests.post(f"{TICKETS_URL}/tickets/{ticket['ticket_id']}/comments",
                        headers=bearer, json={})
    assert res.status_code == 400

def test_add_comment_returns_201(bearer, ticket):
    res = requests.post(f"{TICKETS_URL}/tickets/{ticket['ticket_id']}/comments",
                        headers=bearer, json={"body": "First comment"})
    assert res.status_code == 201, res.text
    c = res.json()
    assert c["comment_id"] != ""
    assert c["body"] == "First comment"
    assert c["ticket_id"] == ticket["ticket_id"]

def test_add_comment_appears_on_get(bearer, ticket):
    requests.post(f"{TICKETS_URL}/tickets/{ticket['ticket_id']}/comments",
                  headers=bearer, json={"body": "Visible comment"})
    res = requests.get(f"{TICKETS_URL}/tickets/{ticket['ticket_id']}", headers=bearer)
    assert res.status_code == 200
    bodies = [c["body"] for c in res.json()["comments"]]
    assert "Visible comment" in bodies

def test_add_comment_to_nonexistent_ticket(bearer):
    res = requests.post(
        f"{TICKETS_URL}/tickets/00000000-0000-0000-0000-000000000000/comments",
        headers=bearer, json={"body": "ghost"},
    )
    assert res.status_code == 404

def test_add_comment_to_other_users_ticket(bearer, other_bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "Comment isolation"})
    tid = res.json()["ticket_id"]

    res = requests.post(f"{TICKETS_URL}/tickets/{tid}/comments",
                        headers=other_bearer, json={"body": "Intrusion"})
    assert res.status_code == 404

    requests.delete(f"{TICKETS_URL}/tickets/{tid}", headers=bearer)


# ── Delete comment ─────────────────────────────────────────────────────────────

def test_delete_comment_not_found(bearer, ticket):
    res = requests.delete(
        f"{TICKETS_URL}/tickets/{ticket['ticket_id']}/comments/00000000-0000-0000-0000-000000000000",
        headers=bearer,
    )
    assert res.status_code == 404

def test_delete_own_comment(bearer, ticket):
    res = requests.post(f"{TICKETS_URL}/tickets/{ticket['ticket_id']}/comments",
                        headers=bearer, json={"body": "Delete me"})
    cid = res.json()["comment_id"]

    res = requests.delete(
        f"{TICKETS_URL}/tickets/{ticket['ticket_id']}/comments/{cid}",
        headers=bearer,
    )
    assert res.status_code == 204

    res = requests.get(f"{TICKETS_URL}/tickets/{ticket['ticket_id']}", headers=bearer)
    cids = [c["comment_id"] for c in res.json()["comments"]]
    assert cid not in cids

def test_delete_comment_other_user_cannot(bearer, other_bearer):
    res = requests.post(f"{TICKETS_URL}/tickets", headers=bearer,
                        json={"title": "Comment delete isolation"})
    tid = res.json()["ticket_id"]
    res = requests.post(f"{TICKETS_URL}/tickets/{tid}/comments",
                        headers=bearer, json={"body": "My comment"})
    cid = res.json()["comment_id"]

    res = requests.delete(
        f"{TICKETS_URL}/tickets/{tid}/comments/{cid}",
        headers=other_bearer,
    )
    assert res.status_code == 404

    requests.delete(f"{TICKETS_URL}/tickets/{tid}", headers=bearer)
