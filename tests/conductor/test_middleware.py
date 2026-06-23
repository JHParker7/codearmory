"""Integration tests for conductor's auth middleware.

Conductor forwards public routes (e.g. /gatekeeper/signup, /gatekeeper/login)
to Gatekeeper without any auth check. Every other non-public path goes through
checkUserAuth, which:
  1. Requires an Authorization: Bearer <token> header (or armory_session cookie).
  2. Decodes the JWT payload locally (no signature check) to validate its
     structure and extract the sub (user ID). Malformed or expired tokens are
     rejected here with 401 without calling Gatekeeper.
  3. For a well-formed, unexpired token, calls POST /check_permissions on
     Gatekeeper with the (service, action, resource) for the route. Gatekeeper
     verifies the signature and evaluates RBAC.
  4. Returns 401 if Gatekeeper reports the token is invalid, 403 if it is valid
     but unauthorized, and forwards the request on success.
  5. The suspicious-activity counter only increments when a well-formed,
     unexpired token reaches Gatekeeper and fails (401 or 403). Requests with no
     token, a malformed token, or an expired token do not count. On success the
     source IP's counter is reset. After 10 such failures from one source IP,
     that IP is blocked for one hour (non-public routes then return 403).
"""

import base64
import json
import uuid
import requests


def bearer(token):
    return {"Authorization": f"Bearer {token}"}


def rand_id():
    return uuid.uuid4().hex


# ---------------------------------------------------------------------------
# Public routes — no auth required
# ---------------------------------------------------------------------------


class TestPublicRoutes:
    def test_signup_requires_no_auth(self, base_url):
        uid = rand_id()[:8]
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": f"pub_{uid}@example.com",
            "username": f"pub_{uid}",
            "password": "password123",
        })
        assert resp.status_code == 201

    def test_login_requires_no_auth(self, base_url, new_user):
        resp = requests.post(f"{base_url}/gatekeeper/login", json={
            "email": new_user["email"],
            "password": new_user["password"],
        })
        assert resp.status_code == 200

    def test_signup_returns_user_id(self, base_url):
        uid = rand_id()[:8]
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": f"pub2_{uid}@example.com",
            "username": f"pub2_{uid}",
            "password": "password123",
        })
        assert "user_id" in resp.json()

    def test_login_returns_token(self, base_url, new_user):
        resp = requests.post(f"{base_url}/gatekeeper/login", json={
            "email": new_user["email"],
            "password": new_user["password"],
        })
        assert "token" in resp.json()
        assert len(resp.json()["token"].split(".")) == 3


# ---------------------------------------------------------------------------
# Missing or malformed Authorization header
# ---------------------------------------------------------------------------


class TestMissingAuth:
    """Every non-public route must reject requests without a Bearer token."""

    def _assert_401(self, base_url, method, path):
        resp = getattr(requests, method)(f"{base_url}{path}")
        assert resp.status_code == 401, f"{method.upper()} {path} → {resp.status_code}"

    def test_no_header_on_get_user(self, base_url):
        self._assert_401(base_url, "get", f"/gatekeeper/users/{rand_id()}")

    def test_no_header_on_get_orgs(self, base_url):
        self._assert_401(base_url, "get", "/gatekeeper/orgs")

    def test_no_header_on_post_orgs(self, base_url):
        self._assert_401(base_url, "post", "/gatekeeper/orgs")

    def test_no_header_on_forge_route(self, base_url):
        self._assert_401(base_url, "get", "/forge/executions")

    def test_non_bearer_scheme_returns_401(self, base_url):
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{rand_id()}",
            headers={"Authorization": "Token abc123"},
        )
        assert resp.status_code == 401

    def test_basic_scheme_returns_401(self, base_url):
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{rand_id()}",
            headers={"Authorization": "Basic dXNlcjpwYXNz"},
        )
        assert resp.status_code == 401

    def test_empty_authorization_header_returns_401(self, base_url):
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{rand_id()}",
            headers={"Authorization": ""},
        )
        assert resp.status_code == 401


# ---------------------------------------------------------------------------
# Malformed JWT — conductor rejects before calling Gatekeeper
# ---------------------------------------------------------------------------


class TestMalformedToken:
    def test_non_jwt_string_returns_401(self, base_url):
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{rand_id()}",
            headers={"Authorization": "Bearer notajwt"},
        )
        assert resp.status_code == 401

    def test_two_segment_jwt_returns_401(self, base_url):
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{rand_id()}",
            headers={"Authorization": "Bearer header.payload"},
        )
        assert resp.status_code == 401

    def test_four_segment_jwt_returns_401(self, base_url):
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{rand_id()}",
            headers={"Authorization": "Bearer a.b.c.d"},
        )
        assert resp.status_code == 401

    def test_invalid_base64_payload_returns_401(self, base_url):
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{rand_id()}",
            headers={"Authorization": "Bearer header.!!!invalid!!!.signature"},
        )
        assert resp.status_code == 401

    def test_payload_without_sub_claim_returns_401(self, base_url):
        payload = base64.urlsafe_b64encode(
            json.dumps({"iss": "test", "iat": 1234567890}).encode()
        ).rstrip(b"=").decode()
        token = f"eyJhbGciOiJFUzI1NiJ9.{payload}.fakesig"
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{rand_id()}",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 401

    def test_payload_with_empty_sub_returns_401(self, base_url):
        payload = base64.urlsafe_b64encode(
            json.dumps({"sub": ""}).encode()
        ).rstrip(b"=").decode()
        token = f"eyJhbGciOiJFUzI1NiJ9.{payload}.fakesig"
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{rand_id()}",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 401

    def test_payload_with_nonexistent_sub_returns_401(self, base_url):
        """A well-formed JWT whose sub points to no real user is rejected."""
        payload = base64.urlsafe_b64encode(
            json.dumps({"sub": rand_id()}).encode()
        ).rstrip(b"=").decode()
        token = f"eyJhbGciOiJFUzI1NiJ9.{payload}.fakesig"
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{rand_id()}",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 401


# ---------------------------------------------------------------------------
# Valid token — request is forwarded to the backend
# ---------------------------------------------------------------------------


class TestValidToken:
    def test_valid_token_reaches_gatekeeper(self, base_url, token, new_user):
        """A valid token lets the request through; Gatekeeper returns 200 for own user."""
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{new_user['user_id']}",
            headers=bearer(token),
        )
        assert resp.status_code == 200

    def test_gatekeeper_response_is_proxied(self, base_url, token, new_user):
        """Conductor forwards the full Gatekeeper response body unchanged."""
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{new_user['user_id']}",
            headers=bearer(token),
        )
        body = resp.json()
        assert body["user_id"] == new_user["user_id"]
        assert body["username"] == new_user["username"]

    def test_gatekeeper_permission_denial_is_proxied(self, base_url, token):
        """Conductor forwards 403 from Gatekeeper without interfering."""
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{uuid.uuid4()}",
            headers=bearer(token),
        )
        assert resp.status_code == 403

# ---------------------------------------------------------------------------
# Deleted user — token is rejected after account removal
# ---------------------------------------------------------------------------


class TestDeletedUser:
    def test_deleted_user_token_is_rejected(self, base_url):
        """Once a user is deleted, subsequent requests with their token return 401."""
        uid = rand_id()[:8]
        email = f"del_{uid}@example.com"

        signup = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": email,
            "username": f"del_{uid}",
            "password": "password123",
        })
        assert signup.status_code == 201
        user_id = signup.json()["user_id"]

        login = requests.post(
            f"{base_url}/gatekeeper/login",
            json={"email": email, "password": "password123"},
        )
        assert login.status_code == 200
        token = login.json()["token"]

        # Token works before deletion.
        pre = requests.get(
            f"{base_url}/gatekeeper/users/{user_id}",
            headers=bearer(token),
        )
        assert pre.status_code == 200

        # Delete the user through conductor (proxied to Gatekeeper).
        requests.delete(
            f"{base_url}/gatekeeper/users/{user_id}",
            headers=bearer(token),
        )

        # Conductor's middleware must now reject the stale token.
        post = requests.get(
            f"{base_url}/gatekeeper/users/{user_id}",
            headers=bearer(token),
        )
        assert post.status_code == 401

    def test_deleted_user_cannot_reach_any_backend_route(self, base_url):
        """Stale token is blocked on all route types, not just /users."""
        uid = rand_id()[:8]
        email = f"del2_{uid}@example.com"

        signup = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": email,
            "username": f"del2_{uid}",
            "password": "password123",
        })
        assert signup.status_code == 201
        user_id = signup.json()["user_id"]

        login = requests.post(
            f"{base_url}/gatekeeper/login",
            json={"email": email, "password": "password123"},
        )
        token = login.json()["token"]

        requests.delete(
            f"{base_url}/gatekeeper/users/{user_id}",
            headers=bearer(token),
        )

        for path in ["/gatekeeper/orgs", "/gatekeeper/teams", "/forge/executions"]:
            resp = requests.get(f"{base_url}{path}", headers=bearer(token))
            assert resp.status_code == 401, f"expected 401 for {path}, got {resp.status_code}"
