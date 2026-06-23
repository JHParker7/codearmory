"""Integration tests for conductor's dynamic service proxy.

Conductor routes /{service}/{path} requests by:
  1. Extracting the first path segment as the service name.
  2. Looking up the service URL in the registry cache (refreshed every 30s).
  3. Verifying the caller is a real user via GET /users/{id} on Gatekeeper.
  4. Stripping the service prefix and forwarding the remaining path to the backend.
  5. Each backend is responsible for its own permission checks via Gatekeeper.
  6. Returns 404 if the service or endpoint is not registered.

Registry is pre-seeded (infra/local/registry-manifest.json) with:
  - "forge"      → http://forge:8083
"""

import uuid
import requests


def bearer(token):
    return {"Authorization": f"Bearer {token}"}


def rand_id():
    return uuid.uuid4().hex


# ---------------------------------------------------------------------------
# Unregistered service
# ---------------------------------------------------------------------------


class TestUnregisteredService:
    def test_unknown_service_returns_404(self, base_url, token):
        """A path whose first segment is not a registered service returns 404 from conductor."""
        resp = requests.get(
            f"{base_url}/unknown-svc/foo",
            headers=bearer(token),
        )
        assert resp.status_code == 404

    def test_empty_service_name_returns_404(self, base_url, token):
        """A request with no service segment (bare /) returns 404."""
        resp = requests.get(
            f"{base_url}/",
            headers=bearer(token),
        )
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# Forge proxy
# ---------------------------------------------------------------------------
# Conductor routes /forge/... to forge (http://forge:8083), stripping the
# leading /forge prefix.  The forge default_grants in registry-manifest.json
# grant listExecution (plus create/get/delete) on the user's own
# {username}/forge/executions namespace to every new user, so the gatekeeper
# permission check passes and GET /forge/executions reaches forge.
# These tests verify routing and auth enforcement, not forge execution logic.


class TestForgeProxy:
    def test_forge_list_requires_auth(self, base_url):
        """GET /forge/executions without a token → 401 from conductor."""
        resp = requests.get(f"{base_url}/forge/executions")
        assert resp.status_code == 401

    def test_forge_list_reaches_permission_check(self, base_url, token):
        """GET /forge/executions with a valid token reaches forge and returns 200.

        forge's default_grants give every new user listExecution on their own
        {username}/forge/executions namespace, so the gatekeeper permission
        check passes and forge returns 200 with the (empty) execution list.
        """
        resp = requests.get(
            f"{base_url}/forge/executions",
            headers=bearer(token),
        )
        assert resp.status_code == 200, resp.text

    def test_forge_post_requires_auth(self, base_url):
        """POST /forge/executions without a token → 401 from conductor."""
        resp = requests.post(
            f"{base_url}/forge/executions",
            json={"image": "alpine:3.19", "command": ["echo", "hello"]},
        )
        assert resp.status_code == 401


# ---------------------------------------------------------------------------
# RBAC enforcement
# ---------------------------------------------------------------------------
# The proxy calls GET /check_permissions on gatekeeper before forwarding.
# A user without the required permission receives 403 from conductor; a user
# with the right permission receives a real response from the backend.


class TestRBACEnforcement:
    def test_missing_permission_returns_403(self, base_url, token):
        """A new user has no createRunnerClass grant, so POST /forge/runner-classes
        triggers a gatekeeper permission check that fails; conductor returns 403
        without forwarding.
        """
        resp = requests.post(f"{base_url}/forge/runner-classes", json={"name": "x"}, headers=bearer(token))
        assert resp.status_code == 403

    def test_valid_permission_is_forwarded(self, base_url, token, new_user):
        """A user with permission for their own namespace gets a backend response.

        The response must not be 401, 403, or 503 — it comes from forge.
        """
        resp = requests.get(f"{base_url}/forge/executions", headers=bearer(token))
        assert resp.status_code not in (401, 403, 503)
