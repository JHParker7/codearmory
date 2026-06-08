"""Integration tests for conductor's request routing.

Conductor routes based on path prefix:
  - POST /signup, POST /login            → Gatekeeper (no auth check)
  - /blueprints/state/{username}/{...}   → Blueprints (after user-existence check)
  - /forge/executions/{...}              → Forge (after user-existence check)
  - /gatekeeper/{...} or bare paths      → Gatekeeper (after user-existence check)

The service name is stripped before forwarding, so /blueprints/state/alice/dev
becomes /state/alice/dev when it reaches Blueprints. Permission checks are
performed by each backend, not by conductor.
"""

import uuid
import requests


def bearer(token):
    return {"Authorization": f"Bearer {token}"}


def rand_id():
    return uuid.uuid4().hex


# ---------------------------------------------------------------------------
# Gatekeeper routes
# ---------------------------------------------------------------------------


class TestGatekeeperRouting:
    def test_own_user_returns_gatekeeper_user_shape(self, base_url, token, new_user):
        """GET /users/{id} → Gatekeeper; response contains user_id and username."""
        resp = requests.get(
            f"{base_url}/users/{new_user['user_id']}",
            headers=bearer(token),
        )
        assert resp.status_code == 200
        body = resp.json()
        assert "user_id" in body
        assert "username" in body

    def test_unknown_path_returns_gatekeeper_404(self, base_url, token):
        """An unregistered path is forwarded to Gatekeeper, which returns 404."""
        resp = requests.get(
            f"{base_url}/no/such/path/{rand_id()}",
            headers=bearer(token),
        )
        assert resp.status_code == 404

    def test_orgs_collection_route_reaches_gatekeeper(self, base_url, token):
        """GET /orgs is a Gatekeeper route; a user without permission gets 403."""
        resp = requests.get(f"{base_url}/orgs", headers=bearer(token))
        assert resp.status_code == 403

    def test_post_signup_forwarded_without_auth(self, base_url):
        uid = rand_id()[:8]
        resp = requests.post(f"{base_url}/signup", json={
            "email": f"rt_{uid}@example.com",
            "username": f"rt_{uid}",
            "password": "password123",
        })
        assert resp.status_code == 201

    def test_post_login_forwarded_without_auth(self, base_url, new_user):
        resp = requests.post(f"{base_url}/login", json={
            "email": new_user["email"],
            "password": new_user["password"],
        })
        assert resp.status_code == 200


# ---------------------------------------------------------------------------
# Blueprints routes
# ---------------------------------------------------------------------------
# These tests require Blueprints to be running. Blueprints calls Gatekeeper for
# permission checks and returns 403 when the caller lacks the required permission.
# Conductor strips the /blueprints prefix before forwarding.


class TestBlueprintsRouting:
    def test_user_scoped_state_reaches_blueprints(self, base_url, token, new_user):
        """/state/{username}/{workspace} is routed to Blueprints.

        Users have permission to access their own state namespace, so an empty
        workspace returns 204. Gatekeeper has no /state/ route and would return
        404, so a non-404 here confirms the request reached Blueprints.
        """
        resp = requests.get(
            f"{base_url}/state/{new_user['username']}/dev",
            headers=bearer(token),
        )
        assert resp.status_code != 404

    def test_deep_user_scoped_path_reaches_blueprints(self, base_url, token, new_user):
        resp = requests.get(
            f"{base_url}/state/{new_user['username']}/team/workspace",
            headers=bearer(token),
        )
        # Conductor strips at /state/{username}/{workspace}; extra segments are
        # forwarded as-is and Blueprints returns 404 or 403.
        assert resp.status_code in (403, 404)

    def test_blueprints_lock_route_requires_auth(self, base_url):
        """LOCK on a state path is blocked by conductor when no token is present."""
        resp = requests.request(
            "LOCK",
            f"{base_url}/state/alice/dev",
        )
        assert resp.status_code == 401

    def test_blueprints_unlock_route_requires_auth(self, base_url):
        resp = requests.request(
            "UNLOCK",
            f"{base_url}/state/alice/dev",
        )
        assert resp.status_code == 401

    def test_state_delete_requires_auth(self, base_url):
        resp = requests.delete(f"{base_url}/state/alice/dev")
        assert resp.status_code == 401

    def test_state_post_requires_auth(self, base_url):
        resp = requests.post(f"{base_url}/state/alice/dev", json={})
        assert resp.status_code == 401


# ---------------------------------------------------------------------------
# Conductor-native endpoints — /openapi.json, /docs
# ---------------------------------------------------------------------------


class TestConductorNativeEndpoints:
    def test_openapi_json_returns_200(self, base_url):
        """GET /openapi.json is served by conductor itself; no auth required."""
        resp = requests.get(f"{base_url}/openapi.json")
        assert resp.status_code == 200

    def test_openapi_json_content_type(self, base_url):
        resp = requests.get(f"{base_url}/openapi.json")
        assert "application/json" in resp.headers.get("Content-Type", "")

    def test_openapi_json_has_required_fields(self, base_url):
        resp = requests.get(f"{base_url}/openapi.json")
        body = resp.json()
        assert "openapi" in body
        assert "info" in body
        assert "paths" in body

    def test_docs_returns_200(self, base_url):
        """GET /docs serves the Swagger UI HTML; no auth required."""
        resp = requests.get(f"{base_url}/docs")
        assert resp.status_code == 200

    def test_docs_content_type_is_html(self, base_url):
        resp = requests.get(f"{base_url}/docs")
        assert "text/html" in resp.headers.get("Content-Type", "")

    def test_docs_references_openapi_spec(self, base_url):
        """Swagger UI page should reference /openapi.json."""
        resp = requests.get(f"{base_url}/docs")
        assert "/openapi.json" in resp.text


# ---------------------------------------------------------------------------
# Internal refresh endpoint — POST /internal/refresh
# ---------------------------------------------------------------------------


class TestInternalRefresh:
    def test_no_key_returns_401(self, base_url):
        """POST /internal/refresh without X-Service-Key is rejected."""
        resp = requests.post(f"{base_url}/internal/refresh")
        assert resp.status_code == 401

    def test_wrong_key_returns_401(self, base_url):
        """An incorrect notify key is rejected."""
        resp = requests.post(
            f"{base_url}/internal/refresh",
            headers={"X-Service-Key": "registry:definitely-wrong-key"},
        )
        assert resp.status_code == 401

    def test_bearer_token_returns_401(self, base_url, token):
        """A user bearer token cannot call the internal refresh endpoint."""
        resp = requests.post(
            f"{base_url}/internal/refresh",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 401
