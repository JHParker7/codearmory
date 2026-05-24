"""Integration tests for conductor's request routing.

Conductor routes based on path:
  - POST /signup, POST /login        → Gatekeeper (no auth check)
  - /state/{username}/{workspace}    → Blueprints (after user check)
  - /{org}/state/{team}/{workspace}  → Blueprints (after user check)
  - everything else                  → Gatekeeper (after user check)

Routing is verified by inspecting the response shape. Gatekeeper returns a
JSON body with resource-specific fields (/users → user_id, etc.) and returns
404 for unknown paths. Blueprints has no /users route and returns 403 for
state paths where the caller lacks a blueprints permission.
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

    def test_check_permissions_returns_authorized_field(self, base_url, token, new_user):
        """GET /check_permissions → Gatekeeper; response contains authorized."""
        resp = requests.get(
            f"{base_url}/check_permissions",
            json={
                "service": "gatekeeper",
                "resource": f"gatekeeper/users/{new_user['user_id']}",
                "action": "getUser",
            },
            headers=bearer(token),
        )
        assert resp.status_code == 200
        assert resp.json()["authorized"] is True

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
# These tests require Blueprints to be running. Blueprints checks permissions
# via Gatekeeper and returns 403 when the caller lacks blueprints/* permissions.
# Gatekeeper has no /state/ routes and would return 404, so a 403 here confirms
# the request reached Blueprints.


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

    def test_org_scoped_state_reaches_blueprints(self, base_url, token):
        """/{org}/state/{team}/{workspace} is routed to Blueprints."""
        resp = requests.get(
            f"{base_url}/acme/state/platform/prod",
            headers=bearer(token),
        )
        assert resp.status_code == 403

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
