"""Integration tests for conductor's request routing.

All routes must include a service-name prefix as the first path segment:
  - POST /gatekeeper/signup, POST /gatekeeper/login  → Gatekeeper (no auth check)
  - /forge/executions/{...}                          → Forge
  - /gatekeeper/{...}                                → Gatekeeper

The service name is stripped before forwarding, so /forge/executions becomes
/executions when it reaches Forge. Conductor enforces RBAC
by calling Gatekeeper's POST /check_permissions before forwarding each request.
"""

import os
import uuid
import pytest
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
        """GET /gatekeeper/orgs is a Gatekeeper route reachable through Conductor.
        listOrg is a default-granted, caller-scoped permission (registry RBAC change
        e70d890), so a signed-up user gets 200 with their own (empty) org list — which
        still proves the route reaches Gatekeeper and is permission-checked there."""
        resp = requests.get(f"{base_url}/gatekeeper/orgs", headers=bearer(token))
        assert resp.status_code == 200

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
# Forge routes
# ---------------------------------------------------------------------------
# All forge routes require the /forge service prefix. Conductor strips it before
# forwarding, so /forge/executions becomes /executions at the Forge backend, and
# enforces RBAC (via gatekeeper) before forwarding. A new user's default grants
# include listExecution on their own {username}/forge/executions namespace.


class TestForgeRouting:
    def test_user_executions_reach_forge(self, base_url, token):
        """GET /forge/executions is routed to Forge; a new user can list their own
        (empty) executions, so a 200 confirms the request reached Forge."""
        resp = requests.get(f"{base_url}/forge/executions", headers=bearer(token))
        assert resp.status_code == 200

    def test_specific_execution_reaches_forge(self, base_url, token):
        """A specific (nonexistent) execution id is forwarded; Forge returns 404."""
        resp = requests.get(f"{base_url}/forge/executions/{uuid.uuid4()}", headers=bearer(token))
        assert resp.status_code in (403, 404)

    def test_forge_create_requires_auth(self, base_url):
        resp = requests.post(f"{base_url}/forge/executions", json={})
        assert resp.status_code == 401

    def test_forge_delete_requires_auth(self, base_url):
        resp = requests.delete(f"{base_url}/forge/executions/{uuid.uuid4()}")
        assert resp.status_code == 401

    def test_forge_runner_classes_require_auth(self, base_url):
        resp = requests.get(f"{base_url}/forge/runner-classes")
        assert resp.status_code == 401

    def test_forge_runtime_backends_require_auth(self, base_url):
        resp = requests.get(f"{base_url}/forge/runtime-backends")
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

    def test_correct_key_returns_202(self, base_url):
        """The registry notify key (X-Service-Key: registry:<CONDUCTOR_NOTIFY_KEY>)
        is accepted and triggers an async route-table refresh.

        Without this positive case the 401 tests above pass vacuously: when
        CONDUCTOR_NOTIFY_KEY is unset, conductor rejects every request regardless
        of the header, so the auth logic is never actually exercised.
        """
        notify_key = os.getenv("CONDUCTOR_NOTIFY_KEY")
        if not notify_key:
            pytest.skip("CONDUCTOR_NOTIFY_KEY not set; cannot exercise the accept path")
        resp = requests.post(
            f"{base_url}/internal/refresh",
            headers={"X-Service-Key": f"registry:{notify_key}"},
        )
        assert resp.status_code == 202
