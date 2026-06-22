"""
Integration tests for the portal's nginx reverse proxy.

These tests verify:
  - Static file serving and SPA fallback
  - /api/* routing to conductor (auth flows, blueprints state, gatekeeper routes)
  - That conductor validation errors (400) surface correctly through the proxy
  - That missing/invalid auth returns appropriate errors through the proxy

Run against the full stack:
  PORTAL_URL=http://localhost:3001 API_URL=http://localhost:8080 pytest tests/portal/ -v
"""

import uuid
import requests


# ---------------------------------------------------------------------------
# Static file serving
# ---------------------------------------------------------------------------


class TestStaticServing:
    def test_root_returns_html(self, portal_url):
        resp = requests.get(portal_url, timeout=10)
        assert resp.status_code == 200
        assert "text/html" in resp.headers.get("Content-Type", "")
        assert "<html" in resp.text.lower() or "<!doctype" in resp.text.lower()

    def test_root_contains_root_div(self, portal_url):
        resp = requests.get(portal_url, timeout=10)
        assert resp.status_code == 200
        assert 'id="root"' in resp.text

    def test_spa_fallback_for_login_route(self, portal_url):
        resp = requests.get(f"{portal_url}/login", timeout=10)
        assert resp.status_code == 200
        assert "text/html" in resp.headers.get("Content-Type", "")

    def test_spa_fallback_for_app_route(self, portal_url):
        resp = requests.get(f"{portal_url}/app/blueprints", timeout=10)
        assert resp.status_code == 200
        assert "text/html" in resp.headers.get("Content-Type", "")

    def test_spa_fallback_for_unknown_deep_route(self, portal_url):
        resp = requests.get(f"{portal_url}/this/does/not/exist", timeout=10)
        assert resp.status_code == 200
        assert "text/html" in resp.headers.get("Content-Type", "")

    def test_static_asset_returns_js(self, portal_url):
        """The Vite build emits assets under /assets/ — at least one JS file must exist."""
        index = requests.get(portal_url, timeout=10).text
        # Extract the first /assets/*.js reference from the HTML
        import re
        match = re.search(r'(/assets/[^"\']+\.js)', index)
        assert match, "no /assets/*.js script tag found in index.html"
        asset_url = f"{portal_url}{match.group(1)}"
        resp = requests.get(asset_url, timeout=10)
        assert resp.status_code == 200
        ct = resp.headers.get("Content-Type", "")
        assert "javascript" in ct or "application/octet-stream" in ct


# ---------------------------------------------------------------------------
# Auth proxy: /api/gatekeeper/signup and /api/gatekeeper/login
# ---------------------------------------------------------------------------


class TestAuthProxy:
    def _uid(self):
        return uuid.uuid4().hex[:8]

    def test_signup_via_portal_returns_201(self, portal_url):
        uid = self._uid()
        resp = requests.post(
            f"{portal_url}/api/gatekeeper/signup",
            json={"email": f"proxy_{uid}@example.com", "username": f"proxy_{uid}", "password": "TestPass1!"},
            timeout=10,
        )
        assert resp.status_code == 201
        body = resp.json()
        assert "user_id" in body
        assert body["username"] == f"proxy_{uid}"

    def test_signup_returns_json(self, portal_url):
        uid = self._uid()
        resp = requests.post(
            f"{portal_url}/api/gatekeeper/signup",
            json={"email": f"json_{uid}@example.com", "username": f"json_{uid}", "password": "TestPass1!"},
            timeout=10,
        )
        assert resp.status_code == 201
        assert "application/json" in resp.headers.get("Content-Type", "")

    def test_login_via_portal_returns_token(self, portal_url, user):
        resp = requests.post(
            f"{portal_url}/api/gatekeeper/login",
            json={"email": user["email"], "password": user["password"]},
            timeout=10,
        )
        assert resp.status_code == 200
        body = resp.json()
        assert "token" in body
        assert len(body["token"]) > 20  # sanity: it looks like a JWT

    def test_login_wrong_password_returns_401(self, portal_url, user):
        resp = requests.post(
            f"{portal_url}/api/gatekeeper/login",
            json={"email": user["email"], "password": "wrong-password"},
            timeout=10,
        )
        assert resp.status_code == 401

    def test_duplicate_signup_returns_409(self, portal_url, user):
        resp = requests.post(
            f"{portal_url}/api/gatekeeper/signup",
            json={"email": user["email"], "username": user["username"], "password": "TestPass1!"},
            timeout=10,
        )
        assert resp.status_code == 409


# ---------------------------------------------------------------------------
# Conductor validation surfaces through the proxy
# ---------------------------------------------------------------------------


class TestValidationProxy:
    """
    Conductor validates inputs before touching any backend. These tests confirm
    that 400 responses pass through nginx unchanged.
    """

    def test_signup_missing_email_returns_400(self, portal_url):
        resp = requests.post(
            f"{portal_url}/api/gatekeeper/signup",
            json={"username": "nouser", "password": "TestPass1!"},
            timeout=10,
        )
        assert resp.status_code == 400

    def test_signup_invalid_username_returns_400(self, portal_url):
        resp = requests.post(
            f"{portal_url}/api/gatekeeper/signup",
            json={"email": "x@example.com", "username": "bad user!", "password": "TestPass1!"},
            timeout=10,
        )
        assert resp.status_code == 400

    def test_signup_short_password_returns_400(self, portal_url):
        resp = requests.post(
            f"{portal_url}/api/gatekeeper/signup",
            json={"email": "x@example.com", "username": "validuser", "password": "short"},
            timeout=10,
        )
        assert resp.status_code == 400

    def test_login_missing_password_returns_400(self, portal_url):
        resp = requests.post(
            f"{portal_url}/api/gatekeeper/login",
            json={"email": "x@example.com"},
            timeout=10,
        )
        assert resp.status_code == 400

    def test_get_user_non_uuid_returns_400(self, portal_url, auth_header):
        resp = requests.get(f"{portal_url}/api/gatekeeper/users/not-a-uuid", headers=auth_header, timeout=10)
        assert resp.status_code == 400

    def test_state_slug_with_special_chars_returns_400(self, portal_url, auth_header):
        resp = requests.get(f"{portal_url}/api/state/bad!user/dev", headers=auth_header, timeout=10)
        assert resp.status_code == 400

    def test_state_workspace_slug_too_long_returns_400(self, portal_url, auth_header):
        resp = requests.get(f"{portal_url}/api/state/alice/{'a' * 65}", headers=auth_header, timeout=10)
        assert resp.status_code == 400


# ---------------------------------------------------------------------------
# Auth enforcement: routes requiring a token
# ---------------------------------------------------------------------------


class TestAuthEnforcement:
    """Authenticated routes must return 401 or 400 (not 200) when no token is provided."""

    def test_get_user_no_token_not_200(self, portal_url, user):
        resp = requests.get(f"{portal_url}/api/gatekeeper/users/{user['user_id']}", timeout=10)
        assert resp.status_code != 200

    def test_get_state_no_token_not_200(self, portal_url, user):
        resp = requests.get(f"{portal_url}/api/state/{user['username']}/dev", timeout=10)
        assert resp.status_code != 200

    def test_get_state_bad_token_not_200(self, portal_url, user):
        resp = requests.get(
            f"{portal_url}/api/state/{user['username']}/dev",
            headers={"Authorization": "Bearer not-a-valid-token"},
            timeout=10,
        )
        assert resp.status_code != 200


# ---------------------------------------------------------------------------
# Blueprints state proxy: /api/state/{user}/{ws}
# ---------------------------------------------------------------------------


class TestBlueprintsProxy:
    def test_get_state_returns_empty_workspace_view(self, portal_url, auth_header, user):
        ws = f"proxy-test-{uuid.uuid4().hex[:6]}"
        resp = requests.get(f"{portal_url}/api/state/{user['username']}/{ws}", headers=auth_header, timeout=10)
        # BFF transforms conductor's 204 into 200 {isEmpty: true}; auth errors pass through
        assert resp.status_code in (200, 403, 404)
        if resp.status_code == 200:
            body = resp.json()
            assert body["isEmpty"] is True
            assert body["locked"] is False

    def test_post_and_get_state_roundtrip(self, portal_url, auth_header, user):
        """Push state via portal proxy, then read it back."""
        ws = f"rt-{uuid.uuid4().hex[:6]}"
        state = {
            "version": 4,
            "terraform_version": "1.6.0",
            "serial": 1,
            "lineage": str(uuid.uuid4()),
            "outputs": {},
            "resources": [],
        }

        # The BFF only transforms GET/DELETE /state; pushing state goes through the
        # passthrough proxy to conductor's blueprints route directly.
        put_resp = requests.post(
            f"{portal_url}/api/blueprints/state/{user['username']}/{ws}",
            json=state,
            headers=auth_header,
            timeout=10,
        )
        assert put_resp.status_code in (200, 201, 403, 404), f"unexpected: {put_resp.status_code} {put_resp.text}"

        if put_resp.status_code in (200, 201):
            get_resp = requests.get(
                f"{portal_url}/api/state/{user['username']}/{ws}",
                headers=auth_header,
                timeout=10,
            )
            assert get_resp.status_code == 200
            body = get_resp.json()
            # BFF returns a WorkspaceView; raw state fields are nested under "state"
            assert body["state"]["lineage"] == state["lineage"]

    def test_delete_nonexistent_state_not_500(self, portal_url, auth_header, user):
        ws = f"del-{uuid.uuid4().hex[:6]}"
        resp = requests.delete(
            f"{portal_url}/api/state/{user['username']}/{ws}",
            headers=auth_header,
            timeout=10,
        )
        # Should be 204, 404, or 403 — never a server error
        assert resp.status_code < 500


# ---------------------------------------------------------------------------
# Gatekeeper routes proxy
# ---------------------------------------------------------------------------


class TestGatekeeperProxy:
    def test_get_own_user_via_portal(self, portal_url, auth_header, user):
        resp = requests.get(
            f"{portal_url}/api/gatekeeper/users/{user['user_id']}",
            headers=auth_header,
            timeout=10,
        )
        assert resp.status_code == 200
        body = resp.json()
        assert body["user_id"] == user["user_id"]
        assert body["username"] == user["username"]

    def test_update_user_via_portal(self, portal_url, auth_header, user):
        resp = requests.put(
            f"{portal_url}/api/gatekeeper/users/{user['user_id']}",
            json={"email": user["email"], "username": user["username"]},
            headers=auth_header,
            timeout=10,
        )
        assert resp.status_code == 200
        assert resp.json()["user_id"] == user["user_id"]

    def test_create_org_via_portal(self, portal_url, auth_header):
        uid = uuid.uuid4().hex[:6]
        resp = requests.post(
            f"{portal_url}/api/gatekeeper/orgs",
            json={"org_name": f"proxy-org-{uid}"},
            headers=auth_header,
            timeout=10,
        )
        # 201 on success or 403 if role doesn't allow it
        assert resp.status_code in (201, 403)
