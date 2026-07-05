"""Integration tests for invite-only registration: the sign-up allowlist and the
registration policy toggle.

Requires the compose stack (see tests/README). These tests mutate a single global
policy row, so every test that turns invite-only ON depends on the module-scoped
`reset_policy` fixture to force it back OFF on teardown — otherwise a failure would
leave the shared instance invite-only and break every other signup test (including
the admin_token fixture's own signup). For the same reason this file is not safe to
run under pytest-xdist parallelism.
"""

import uuid
import requests
import pytest


def _auth(admin_token):
    return {"Authorization": f"Bearer {admin_token['token']}"}


def _signup_payload(email):
    uid = uuid.uuid4().hex[:8]
    return {"email": email, "username": f"user_{uid}", "password": "password123"}


@pytest.fixture
def reset_policy(base_url, admin_token):
    """Always restore open registration after a test, even on failure."""
    yield
    requests.put(
        f"{base_url}/signup-policy",
        json={"invite_only": False},
        headers=_auth(admin_token),
    )


class TestSignupAllowlistCRUD:
    def test_add_list_delete(self, base_url, admin_token):
        email = f"allow_{uuid.uuid4().hex[:8]}@example.com"
        resp = requests.post(
            f"{base_url}/signup-allowlist",
            json={"email": email, "note": "itest"},
            headers=_auth(admin_token),
        )
        assert resp.status_code == 201, resp.text
        entry = resp.json()
        assert entry["email"] == email
        entry_id = entry["entry_id"]

        listed = requests.get(f"{base_url}/signup-allowlist", headers=_auth(admin_token))
        assert listed.status_code == 200
        assert any(e["entry_id"] == entry_id for e in listed.json())

        deleted = requests.delete(
            f"{base_url}/signup-allowlist/{entry_id}", headers=_auth(admin_token)
        )
        assert deleted.status_code == 204

        after = requests.get(f"{base_url}/signup-allowlist", headers=_auth(admin_token))
        assert not any(e["entry_id"] == entry_id for e in after.json())

    def test_email_is_normalised_lowercase(self, base_url, admin_token):
        email = f"MixedCase_{uuid.uuid4().hex[:8]}@Example.com"
        resp = requests.post(
            f"{base_url}/signup-allowlist",
            json={"email": email},
            headers=_auth(admin_token),
        )
        assert resp.status_code == 201
        entry_id = resp.json()["entry_id"]
        try:
            assert resp.json()["email"] == email.lower()
        finally:
            requests.delete(
                f"{base_url}/signup-allowlist/{entry_id}", headers=_auth(admin_token)
            )

    def test_duplicate_returns_409(self, base_url, admin_token):
        email = f"dup_{uuid.uuid4().hex[:8]}@example.com"
        first = requests.post(
            f"{base_url}/signup-allowlist",
            json={"email": email},
            headers=_auth(admin_token),
        )
        assert first.status_code == 201
        entry_id = first.json()["entry_id"]
        try:
            dup = requests.post(
                f"{base_url}/signup-allowlist",
                json={"email": email},
                headers=_auth(admin_token),
            )
            assert dup.status_code == 409
        finally:
            requests.delete(
                f"{base_url}/signup-allowlist/{entry_id}", headers=_auth(admin_token)
            )

    def test_invalid_email_returns_400(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/signup-allowlist",
            json={"email": "garbage"},
            headers=_auth(admin_token),
        )
        assert resp.status_code == 400

    def test_requires_admin(self, base_url, token):
        resp = requests.get(
            f"{base_url}/signup-allowlist",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 403


class TestSignupPolicy:
    def test_get_returns_invite_only_flag(self, base_url, admin_token):
        resp = requests.get(f"{base_url}/signup-policy", headers=_auth(admin_token))
        assert resp.status_code == 200
        assert "invite_only" in resp.json()

    def test_toggle_and_read_back(self, base_url, admin_token, reset_policy):
        put = requests.put(
            f"{base_url}/signup-policy",
            json={"invite_only": True},
            headers=_auth(admin_token),
        )
        assert put.status_code == 200
        got = requests.get(f"{base_url}/signup-policy", headers=_auth(admin_token))
        assert got.json()["invite_only"] is True

    def test_setup_status_exposes_invite_only(self, base_url, admin_token, reset_policy):
        requests.put(
            f"{base_url}/signup-policy",
            json={"invite_only": True},
            headers=_auth(admin_token),
        )
        status = requests.get(f"{base_url}/setup/status")
        assert status.status_code == 200
        assert status.json().get("invite_only") is True

    def test_update_requires_admin(self, base_url, token):
        resp = requests.put(
            f"{base_url}/signup-policy",
            json={"invite_only": True},
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 403


class TestInviteOnlyGate:
    def test_blocks_non_allowlisted_email(self, base_url, admin_token, reset_policy):
        requests.put(
            f"{base_url}/signup-policy",
            json={"invite_only": True},
            headers=_auth(admin_token),
        )
        resp = requests.post(
            f"{base_url}/signup",
            json=_signup_payload(f"blocked_{uuid.uuid4().hex[:8]}@example.com"),
        )
        assert resp.status_code == 403

    def test_allows_allowlisted_exact_email(self, base_url, admin_token, reset_policy):
        requests.put(
            f"{base_url}/signup-policy",
            json={"invite_only": True},
            headers=_auth(admin_token),
        )
        email = f"allowed_{uuid.uuid4().hex[:8]}@example.com"
        add = requests.post(
            f"{base_url}/signup-allowlist",
            json={"email": email},
            headers=_auth(admin_token),
        )
        assert add.status_code == 201
        entry_id = add.json()["entry_id"]
        try:
            resp = requests.post(f"{base_url}/signup", json=_signup_payload(email))
            assert resp.status_code == 201, resp.text
        finally:
            requests.delete(
                f"{base_url}/signup-allowlist/{entry_id}", headers=_auth(admin_token)
            )

    def test_open_policy_allows_any_email(self, base_url, admin_token):
        # Baseline: with invite-only off (the default), any valid email signs up.
        requests.put(
            f"{base_url}/signup-policy",
            json={"invite_only": False},
            headers=_auth(admin_token),
        )
        resp = requests.post(
            f"{base_url}/signup",
            json=_signup_payload(f"open_{uuid.uuid4().hex[:8]}@example.com"),
        )
        assert resp.status_code == 201, resp.text
