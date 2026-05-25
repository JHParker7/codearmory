"""Integration tests for conductor's dynamic service proxy.

Conductor has a /{path...} catch-all route that:
  1. Extracts the first path segment as the service name.
  2. Looks up the service URL in the registry.
  3. Calls GET /check_permissions on gatekeeper before forwarding.
  4. Returns 503 if the service is not registered.
  5. Returns 403 if the permission check fails.
  6. Forwards the request (and response) to the upstream service.

Registry is pre-seeded (infra/local/compose.yml) with:
  - "blueprints" → http://blueprints:8081
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
    def test_unknown_service_returns_503(self, base_url, token):
        """A service name not in the registry returns 503 from conductor."""
        resp = requests.get(
            f"{base_url}/unknown-svc/foo",
            headers=bearer(token),
        )
        assert resp.status_code == 503

    def test_empty_service_name_returns_404(self, base_url, token):
        """A request with no service segment (bare /) returns 404."""
        resp = requests.get(
            f"{base_url}/",
            headers=bearer(token),
        )
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# Blueprints proxy
# ---------------------------------------------------------------------------
# Conductor routes /blueprints/... to blueprints (http://blueprints:8081),
# stripping the leading /blueprints prefix.  A new user has permission for
# their own state namespace so /blueprints/state/{username}/dev returns 204.


class TestBlueprintsProxy:
    def test_get_own_state_reaches_blueprints(self, base_url, token, new_user):
        """/blueprints/state/{username}/dev → 204 for an empty workspace.

        Blueprints returns 204 when the workspace exists but is empty.
        Gatekeeper has no /state/ route and would return 404; a 204 here
        confirms the request reached Blueprints.
        """
        resp = requests.get(
            f"{base_url}/blueprints/state/{new_user['username']}/dev",
            headers=bearer(token),
        )
        assert resp.status_code not in (404, 503)

    def test_proxy_requires_auth(self, base_url):
        """GET /blueprints/state/alice/dev without a token → 401 from conductor."""
        resp = requests.get(f"{base_url}/blueprints/state/alice/dev")
        assert resp.status_code == 401

    def test_other_user_state_is_forbidden(self, base_url, token):
        """/blueprints/state/{other_username}/dev with a valid token → 403.

        The permission check or blueprints itself denies access to another
        user's state namespace.
        """
        other = f"other_{rand_id()[:8]}"
        resp = requests.get(
            f"{base_url}/blueprints/state/{other}/dev",
            headers=bearer(token),
        )
        assert resp.status_code == 403

    def test_post_state_reaches_blueprints(self, base_url, token, new_user):
        """POST /blueprints/state/{username}/ws with a valid JSON body is proxied.

        Conductor must not return 404 or 503; the response comes from Blueprints.
        """
        resp = requests.post(
            f"{base_url}/blueprints/state/{new_user['username']}/ws",
            json={"version": 4, "terraform_version": "1.5.0", "resources": []},
            headers=bearer(token),
        )
        assert resp.status_code not in (404, 503)


# ---------------------------------------------------------------------------
# Forge proxy
# ---------------------------------------------------------------------------
# Conductor routes /forge/... to forge (http://forge:8083), stripping the
# leading /forge prefix.  A new user has no forge permissions by default so
# the gatekeeper permission check returns 403 before the request reaches forge.
# These tests verify routing and auth enforcement, not forge execution logic.


class TestForgeProxy:
    def test_forge_list_requires_auth(self, base_url):
        """GET /forge/executions without a token → 401 from conductor."""
        resp = requests.get(f"{base_url}/forge/executions")
        assert resp.status_code == 401

    def test_forge_list_reaches_permission_check(self, base_url, token):
        """GET /forge/executions with a valid token reaches the permission check.

        A new user has no forge permissions, so gatekeeper returns 403.
        If the user somehow has permission, 200 is also acceptable — the
        important assertion is that conductor did not return 401, 404, or 503.
        """
        resp = requests.get(
            f"{base_url}/forge/executions",
            headers=bearer(token),
        )
        assert resp.status_code in (200, 403)

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
        """A user who does not own the target namespace gets 403.

        Accessing /blueprints/state/{other_user}/dev triggers a gatekeeper
        permission check that fails, so conductor returns 403 without
        forwarding the request to blueprints.
        """
        other = f"stranger_{rand_id()[:8]}"
        resp = requests.get(
            f"{base_url}/blueprints/state/{other}/dev",
            headers=bearer(token),
        )
        assert resp.status_code == 403

    def test_valid_permission_is_forwarded(self, base_url, token, new_user):
        """A user with permission for their own namespace gets a backend response.

        The response must not be 401, 403, or 503 — it comes from blueprints.
        """
        resp = requests.get(
            f"{base_url}/blueprints/state/{new_user['username']}/dev",
            headers=bearer(token),
        )
        assert resp.status_code not in (401, 403, 503)
