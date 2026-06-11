"""Integration tests for conductor's request routing.

All routes must include a service-name prefix as the first path segment:
  - POST /gatekeeper/signup, POST /gatekeeper/login  → Gatekeeper (no auth check)
  - /blueprints/state/{username}/{...}               → Blueprints
  - /forge/executions/{...}                          → Forge
  - /gatekeeper/{...}                                → Gatekeeper

The service name is stripped before forwarding, so /blueprints/state/alice/dev
becomes /state/alice/dev when it reaches Blueprints. Conductor enforces RBAC
by calling Gatekeeper's POST /check_permissions before forwarding each request.
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
        """GET /gatekeeper/users/{id} → Gatekeeper; response contains user_id and username."""
        resp = requests.get(
            f"{base_url}/gatekeeper/users/{new_user['user_id']}",
            headers=bearer(token),
        )
        assert resp.status_code == 200
        body = resp.json()
        assert "user_id" in body
        assert "username" in body

    def test_unknown_service_returns_404(self, base_url, token):
        """A path whose first segment is not a registered service name returns 404."""
        resp = requests.get(
            f"{base_url}/no/such/path/{rand_id()}",
            headers=bearer(token),
        )
        assert resp.status_code == 404

    def test_orgs_collection_route_reaches_gatekeeper(self, base_url, token):
        """GET /gatekeeper/orgs is a Gatekeeper route; a user without listOrg permission gets 403."""
        resp = requests.get(f"{base_url}/gatekeeper/orgs", headers=bearer(token))
        assert resp.status_code == 403

    def test_post_signup_forwarded_without_auth(self, base_url):
        uid = rand_id()[:8]
        resp = requests.post(f"{base_url}/gatekeeper/signup", json={
            "email": f"rt_{uid}@example.com",
            "username": f"rt_{uid}",
            "password": "password123",
        })
        assert resp.status_code == 201

    def test_post_login_forwarded_without_auth(self, base_url, new_user):
        resp = requests.post(f"{base_url}/gatekeeper/login", json={
            "email": new_user["email"],
            "password": new_user["password"],
        })
        assert resp.status_code == 200


# ---------------------------------------------------------------------------
# Blueprints routes
# ---------------------------------------------------------------------------
# All blueprints routes require the /blueprints service prefix. Conductor strips
# it before forwarding, so /blueprints/state/alice/dev becomes /state/alice/dev
# at the Blueprints backend. Conductor enforces RBAC before forwarding.


class TestBlueprintsRouting:
    def test_user_scoped_state_reaches_blueprints(self, base_url, token, new_user):
        """/blueprints/state/{username}/{workspace} is routed to Blueprints.

        Users have permission to access their own state namespace, so an empty
        workspace returns 204. Gatekeeper has no /state/ route and would return
        404, so a non-404 here confirms the request reached Blueprints.
        """
        resp = requests.get(
            f"{base_url}/blueprints/state/{new_user['username']}/dev",
            headers=bearer(token),
        )
        assert resp.status_code != 404

    def test_deep_user_scoped_path_reaches_blueprints(self, base_url, token, new_user):
        resp = requests.get(
            f"{base_url}/blueprints/state/{new_user['username']}/team/workspace",
            headers=bearer(token),
        )
        # Extra path segments beyond {workspace} are forwarded as-is;
        # Blueprints returns 404 or 403 for unrecognised sub-paths.
        assert resp.status_code in (403, 404)

    def test_blueprints_lock_route_requires_auth(self, base_url):
        """LOCK on a state path is blocked by conductor when no token is present."""
        resp = requests.request(
            "LOCK",
            f"{base_url}/blueprints/state/alice/dev",
        )
        assert resp.status_code == 401

    def test_blueprints_unlock_route_requires_auth(self, base_url):
        resp = requests.request(
            "UNLOCK",
            f"{base_url}/blueprints/state/alice/dev",
        )
        assert resp.status_code == 401

    def test_state_delete_requires_auth(self, base_url):
        resp = requests.delete(f"{base_url}/blueprints/state/alice/dev")
        assert resp.status_code == 401

    def test_state_post_requires_auth(self, base_url):
        resp = requests.post(f"{base_url}/blueprints/state/alice/dev", json={})
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

    def test_docs_lists_services(self, base_url):
        """Docs page lists registered services with links to per-service docs."""
        resp = requests.get(f"{base_url}/docs")
        assert "/docs/" in resp.text


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
