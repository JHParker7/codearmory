"""Integration tests for the /account endpoints."""
import pytest
import requests

from conftest import GITEA_INTEGRATION_URL, _signup_login, _create_forgejo_user, _delete_forgejo_user


@pytest.fixture(scope="module")
def acct_bearer():
    """Isolated CA user for account lifecycle tests."""
    return {"Authorization": f"Bearer {_signup_login('acct')}"}


@pytest.fixture(scope="module")
def acct_fg(acct_bearer):
    """Forgejo user paired with acct_bearer; ensures account is unlinked on entry and exit."""
    user = _create_forgejo_user()
    requests.delete(f"{GITEA_INTEGRATION_URL}/account", headers=acct_bearer)
    yield user
    requests.delete(f"{GITEA_INTEGRATION_URL}/account", headers=acct_bearer)
    _delete_forgejo_user(user["username"])


# ── auth ───────────────────────────────────────────────────────────────────────

def test_get_account_no_auth():
    res = requests.get(f"{GITEA_INTEGRATION_URL}/account")
    assert res.status_code == 401

def test_link_account_no_auth():
    res = requests.put(f"{GITEA_INTEGRATION_URL}/account", json={"gitea_username": "x", "gitea_token": "y"})
    assert res.status_code == 401

def test_unlink_account_no_auth():
    res = requests.delete(f"{GITEA_INTEGRATION_URL}/account")
    assert res.status_code == 401


# ── unlinked state ─────────────────────────────────────────────────────────────

def test_get_account_not_linked(acct_bearer):
    res = requests.get(f"{GITEA_INTEGRATION_URL}/account", headers=acct_bearer)
    assert res.status_code == 404

def test_unlink_account_not_linked(acct_bearer):
    res = requests.delete(f"{GITEA_INTEGRATION_URL}/account", headers=acct_bearer)
    assert res.status_code == 404


# ── link validation ────────────────────────────────────────────────────────────

def test_link_missing_username(acct_bearer, acct_fg):
    res = requests.put(f"{GITEA_INTEGRATION_URL}/account",
                       headers=acct_bearer,
                       json={"gitea_token": acct_fg["token"]})
    assert res.status_code == 400

def test_link_missing_token(acct_bearer, acct_fg):
    res = requests.put(f"{GITEA_INTEGRATION_URL}/account",
                       headers=acct_bearer,
                       json={"gitea_username": acct_fg["username"]})
    assert res.status_code == 400

def test_link_invalid_token(acct_bearer, acct_fg):
    res = requests.put(f"{GITEA_INTEGRATION_URL}/account",
                       headers=acct_bearer,
                       json={"gitea_username": acct_fg["username"], "gitea_token": "not-a-real-token"})
    assert res.status_code == 422

def test_link_username_mismatch(acct_bearer, acct_fg):
    res = requests.put(f"{GITEA_INTEGRATION_URL}/account",
                       headers=acct_bearer,
                       json={"gitea_username": "definitely-wrong-name", "gitea_token": acct_fg["token"]})
    assert res.status_code == 422


# ── link lifecycle ─────────────────────────────────────────────────────────────

def test_link_account_returns_201(acct_bearer, acct_fg):
    res = requests.put(f"{GITEA_INTEGRATION_URL}/account",
                       headers=acct_bearer,
                       json={"gitea_username": acct_fg["username"], "gitea_token": acct_fg["token"]})
    assert res.status_code == 201

def test_link_account_response_shape(acct_bearer, acct_fg):
    res = requests.put(f"{GITEA_INTEGRATION_URL}/account",
                       headers=acct_bearer,
                       json={"gitea_username": acct_fg["username"], "gitea_token": acct_fg["token"]})
    body = res.json()
    assert "user_id" in body
    assert body["gitea_username"] == acct_fg["username"]
    assert "created_at" in body
    assert "updated_at" in body

def test_get_account_after_link(acct_bearer, acct_fg):
    requests.put(f"{GITEA_INTEGRATION_URL}/account",
                 headers=acct_bearer,
                 json={"gitea_username": acct_fg["username"], "gitea_token": acct_fg["token"]})
    res = requests.get(f"{GITEA_INTEGRATION_URL}/account", headers=acct_bearer)
    assert res.status_code == 200
    assert res.json()["gitea_username"] == acct_fg["username"]

def test_relink_account_returns_200(acct_bearer, acct_fg):
    requests.put(f"{GITEA_INTEGRATION_URL}/account",
                 headers=acct_bearer,
                 json={"gitea_username": acct_fg["username"], "gitea_token": acct_fg["token"]})
    res = requests.put(f"{GITEA_INTEGRATION_URL}/account",
                       headers=acct_bearer,
                       json={"gitea_username": acct_fg["username"], "gitea_token": acct_fg["token"]})
    assert res.status_code == 200

def test_unlink_account_returns_204(acct_bearer, acct_fg):
    requests.put(f"{GITEA_INTEGRATION_URL}/account",
                 headers=acct_bearer,
                 json={"gitea_username": acct_fg["username"], "gitea_token": acct_fg["token"]})
    res = requests.delete(f"{GITEA_INTEGRATION_URL}/account", headers=acct_bearer)
    assert res.status_code == 204

def test_get_account_after_unlink(acct_bearer, acct_fg):
    requests.put(f"{GITEA_INTEGRATION_URL}/account",
                 headers=acct_bearer,
                 json={"gitea_username": acct_fg["username"], "gitea_token": acct_fg["token"]})
    requests.delete(f"{GITEA_INTEGRATION_URL}/account", headers=acct_bearer)
    res = requests.get(f"{GITEA_INTEGRATION_URL}/account", headers=acct_bearer)
    assert res.status_code == 404
