"""Integration tests for conductor's input validation.

Conductor validates inputs before forwarding to any backend:
  - POST /signup: email, username, password fields + format checks
  - POST /login: email, password fields
  - Authenticated routes with {id} path params: UUID format
"""

import uuid
import requests


def bearer(token):
    return {"Authorization": f"Bearer {token}"}


def rand_id():
    return uuid.uuid4().hex


# ---------------------------------------------------------------------------
# POST /signup body validation
# ---------------------------------------------------------------------------


class TestSignupValidation:
    def test_missing_email_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "username": "validuser",
            "password": "password123",
        })
        assert resp.status_code == 400

    def test_missing_username_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": "a@example.com",
            "password": "password123",
        })
        assert resp.status_code == 400

    def test_missing_password_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": "a@example.com",
            "username": "validuser",
        })
        assert resp.status_code == 400

    def test_empty_email_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": "",
            "username": "validuser",
            "password": "password123",
        })
        assert resp.status_code == 400

    def test_invalid_email_format_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": "notanemail",
            "username": "validuser",
            "password": "password123",
        })
        assert resp.status_code == 400

    def test_email_missing_tld_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": "user@nodot",
            "username": "validuser",
            "password": "password123",
        })
        assert resp.status_code == 400

    def test_username_with_spaces_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": "a@example.com",
            "username": "bad user",
            "password": "password123",
        })
        assert resp.status_code == 400

    def test_username_with_special_chars_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": "a@example.com",
            "username": "bad@user!",
            "password": "password123",
        })
        assert resp.status_code == 400

    def test_username_too_long_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": "a@example.com",
            "username": "a" * 65,
            "password": "password123",
        })
        assert resp.status_code == 400

    def test_password_too_short_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": "a@example.com",
            "username": "validuser",
            "password": "short",
        })
        assert resp.status_code == 400

    def test_non_json_body_returns_400(self, base_url):
        resp = requests.post(
            f"{base_url}/gatekeeper/signup",
            data="not json",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status_code == 400

    def test_body_too_large_returns_413(self, base_url):
        resp = requests.post(
            f"{base_url}/gatekeeper/signup",
            data="x" * (65 * 1024),
            headers={"Content-Type": "application/json"},
        )
        assert resp.status_code == 413

    def test_valid_signup_still_succeeds(self, base_url):
        uid = rand_id()[:8]
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": f"val_{uid}@example.com",
            "username": f"val_{uid}",
            "password": "password123",
        })
        assert resp.status_code == 201

    def test_username_with_hyphens_and_underscores_accepted(self, base_url):
        uid = rand_id()[:6]
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": f"slug_{uid}@example.com",
            "username": f"slug-{uid}_ok",
            "password": "password123",
        })
        assert resp.status_code == 201


# ---------------------------------------------------------------------------
# POST /login body validation
# ---------------------------------------------------------------------------


class TestLoginValidation:
    def test_missing_email_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/login", json={"password": "password123"})
        assert resp.status_code == 400

    def test_missing_password_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/login", json={"email": "a@example.com"})
        assert resp.status_code == 400

    def test_empty_email_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/login", json={"email": "", "password": "password123"})
        assert resp.status_code == 400

    def test_empty_password_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/gatekeeper/login", json={"email": "a@example.com", "password": ""})
        assert resp.status_code == 400

    def test_non_json_body_returns_400(self, base_url):
        resp = requests.post(
            f"{base_url}/gatekeeper/login",
            data="not json",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status_code == 400

    def test_valid_login_still_succeeds(self, base_url, new_user):
        resp = requests.post(f"{base_url}/gatekeeper/login", json={
            "email": new_user["email"],
            "password": new_user["password"],
        })
        assert resp.status_code == 200


# ---------------------------------------------------------------------------
# UUID path parameter validation — Gatekeeper routes
# ---------------------------------------------------------------------------


class TestUUIDPathValidation:
    """Conductor must reject non-UUID {id} segments with 400 before forwarding."""

    def _check(self, base_url, token, method, path):
        resp = getattr(requests, method)(f"{base_url}{path}", headers=bearer(token))
        assert resp.status_code == 400, f"{method.upper()} {path} → {resp.status_code}"

    def test_get_user_non_uuid_returns_400(self, base_url, token):
        self._check(base_url, token, "get", "/gatekeeper/users/not-a-uuid")

    def test_get_user_numeric_id_returns_400(self, base_url, token):
        self._check(base_url, token, "get", "/gatekeeper/users/12345")

    def test_delete_user_non_uuid_returns_400(self, base_url, token):
        self._check(base_url, token, "delete", "/gatekeeper/users/not-a-uuid")

    def test_get_org_non_uuid_returns_400(self, base_url, token):
        self._check(base_url, token, "get", "/gatekeeper/orgs/not-a-uuid")

    def test_get_team_non_uuid_returns_400(self, base_url, token):
        self._check(base_url, token, "get", "/gatekeeper/teams/not-a-uuid")

    def test_get_role_non_uuid_returns_400(self, base_url, token):
        self._check(base_url, token, "get", "/gatekeeper/roles/not-a-uuid")

    def test_get_permissions_non_uuid_returns_400(self, base_url, token):
        self._check(base_url, token, "get", "/gatekeeper/permissions/not-a-uuid")

    def test_get_session_non_uuid_returns_400(self, base_url, token):
        self._check(base_url, token, "get", "/gatekeeper/sessions/not-a-uuid")

    def test_get_invite_non_uuid_returns_400(self, base_url, token):
        self._check(base_url, token, "get", "/gatekeeper/invites/not-a-uuid")

    def test_valid_uuid_is_forwarded(self, base_url, token, new_user):
        """A valid UUID passes conductor's check; Gatekeeper decides the outcome."""
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{new_user['user_id']}",
            headers=bearer(token),
        )
        assert resp.status_code != 400


# ---------------------------------------------------------------------------
# JSON body validation — authenticated Gatekeeper routes
# ---------------------------------------------------------------------------


class TestJSONBodyValidation:
    """Conductor must reject malformed JSON on POST/PUT routes before forwarding."""

    def _post(self, base_url, token, path, body, content_type="application/json"):
        return requests.post(
            f"{base_url}{path}",
            data=body,
            headers={**bearer(token), "Content-Type": content_type},
        )

    def _put(self, base_url, token, path, body):
        return requests.put(
            f"{base_url}{path}",
            data=body,
            headers={**bearer(token), "Content-Type": "application/json"},
        )

    def test_post_orgs_invalid_json_returns_400(self, base_url, token):
        resp = self._post(base_url, token, "/gatekeeper/orgs", b"not json")
        assert resp.status_code == 400

    def test_post_orgs_truncated_json_returns_400(self, base_url, token):
        resp = self._post(base_url, token, "/gatekeeper/orgs", b'{"name":')
        assert resp.status_code == 400

    def test_put_user_invalid_json_returns_400(self, base_url, token, new_user):
        resp = self._put(base_url, token, f"/gatekeeper/users/{new_user['user_id']}", b"not json")
        assert resp.status_code == 400

    def test_post_teams_invalid_json_returns_400(self, base_url, token):
        resp = self._post(base_url, token, "/gatekeeper/teams", b"[unclosed")
        assert resp.status_code == 400

    def test_post_roles_invalid_json_returns_400(self, base_url, token):
        resp = self._post(base_url, token, "/gatekeeper/roles", b"not json at all")
        assert resp.status_code == 400

    def test_post_permissions_invalid_json_returns_400(self, base_url, token):
        resp = self._post(base_url, token, "/gatekeeper/permissions", b"not json")
        assert resp.status_code == 400

    def test_put_org_invalid_json_returns_400(self, base_url, token, new_user):
        # Use a random UUID — conductor validates JSON before touching the backend.
        import uuid
        resp = self._put(base_url, token, f"/gatekeeper/orgs/{uuid.uuid4()}", b"bad json")
        assert resp.status_code == 400

    def test_valid_json_body_is_forwarded(self, base_url, token):
        """A syntactically valid JSON body passes conductor; backend decides the outcome."""
        resp = self._post(base_url, token, "/gatekeeper/orgs", b'{"org_name": "test-org"}')
        assert resp.status_code != 400

