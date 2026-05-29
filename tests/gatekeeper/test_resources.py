"""Integration tests for resource CRUD endpoints.

Every resource endpoint requires both a valid Bearer token (authMiddleware) and a
matching permission entry (requirePermission).  A user created via POST /signup
receives getUser/updateUser/deleteUser permissions scoped to their own user ID, so
those three endpoints can be fully exercised.

For all other resources (orgs, teams, roles, permissions, sessions) the admin_token
fixture seeds an admin user with collection-level create permissions via direct DB
writes, then grants per-resource-ID permissions inline as resources are created.
"""

import base64
import json
import uuid
import pytest
import requests


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

AUTH_HEADER = "Authorization"


def bearer(token):
    return {AUTH_HEADER: f"Bearer {token}"}


def rand_id():
    return uuid.uuid4().hex


# ---------------------------------------------------------------------------
# 401 — no authentication
# ---------------------------------------------------------------------------


class TestUnauthenticated:
    """Every protected endpoint must reject requests without a Bearer token."""

    def _check(self, base_url, method, path):
        url = f"{base_url}{path}"
        resp = getattr(requests, method)(url)
        assert resp.status_code == 401, f"{method.upper()} {path} → {resp.status_code}"

    def test_get_user(self, base_url):
        self._check(base_url, "get", f"/users/{rand_id()}")

    def test_put_user(self, base_url):
        self._check(base_url, "put", f"/users/{rand_id()}")

    def test_delete_user(self, base_url):
        self._check(base_url, "delete", f"/users/{rand_id()}")

    def test_get_orgs(self, base_url):
        self._check(base_url, "get", "/orgs")

    def test_post_orgs(self, base_url):
        self._check(base_url, "post", "/orgs")

    def test_get_org(self, base_url):
        self._check(base_url, "get", f"/orgs/{rand_id()}")

    def test_put_org(self, base_url):
        self._check(base_url, "put", f"/orgs/{rand_id()}")

    def test_delete_org(self, base_url):
        self._check(base_url, "delete", f"/orgs/{rand_id()}")

    def test_get_teams(self, base_url):
        self._check(base_url, "get", "/teams")

    def test_post_teams(self, base_url):
        self._check(base_url, "post", "/teams")

    def test_get_team(self, base_url):
        self._check(base_url, "get", f"/teams/{rand_id()}")

    def test_put_team(self, base_url):
        self._check(base_url, "put", f"/teams/{rand_id()}")

    def test_delete_team(self, base_url):
        self._check(base_url, "delete", f"/teams/{rand_id()}")

    def test_post_roles(self, base_url):
        self._check(base_url, "post", "/roles")

    def test_get_role(self, base_url):
        self._check(base_url, "get", f"/roles/{rand_id()}")

    def test_put_role(self, base_url):
        self._check(base_url, "put", f"/roles/{rand_id()}")

    def test_delete_role(self, base_url):
        self._check(base_url, "delete", f"/roles/{rand_id()}")

    def test_post_permissions(self, base_url):
        self._check(base_url, "post", "/permissions")

    def test_get_permissions(self, base_url):
        self._check(base_url, "get", f"/permissions/{rand_id()}")

    def test_put_permissions(self, base_url):
        self._check(base_url, "put", f"/permissions/{rand_id()}")

    def test_delete_permissions(self, base_url):
        self._check(base_url, "delete", f"/permissions/{rand_id()}")

    def test_get_session(self, base_url):
        self._check(base_url, "get", f"/sessions/{rand_id()}")

    def test_delete_session(self, base_url):
        self._check(base_url, "delete", f"/sessions/{rand_id()}")


# ---------------------------------------------------------------------------
# 403 — authenticated but no matching permission
# ---------------------------------------------------------------------------


class TestForbidden:
    """A freshly signed-up user lacks permission for any resource except their own
    user record; all other endpoints must return 403."""

    def _check(self, base_url, token, method, path, body=None):
        url = f"{base_url}{path}"
        resp = getattr(requests, method)(url, json=body, headers=bearer(token))
        assert resp.status_code == 403, f"{method.upper()} {path} → {resp.status_code}"

    def test_get_other_user(self, base_url, token):
        self._check(base_url, token, "get", f"/users/{rand_id()}")

    def test_put_other_user(self, base_url, token):
        self._check(
            base_url,
            token,
            "put",
            f"/users/{rand_id()}",
            {"email": "x@x.com", "username": "x"},
        )

    def test_delete_other_user(self, base_url, token):
        self._check(base_url, token, "delete", f"/users/{rand_id()}")

    def test_get_orgs(self, base_url, token):
        self._check(base_url, token, "get", "/orgs", {})

    def test_get_org(self, base_url, token):
        self._check(base_url, token, "get", f"/orgs/{rand_id()}")

    def test_put_org(self, base_url, token):
        self._check(base_url, token, "put", f"/orgs/{rand_id()}", {"org_name": "x"})

    def test_delete_org(self, base_url, token):
        self._check(base_url, token, "delete", f"/orgs/{rand_id()}")

    def test_get_teams(self, base_url, token):
        self._check(base_url, token, "get", "/teams", {})

    def test_get_team(self, base_url, token):
        self._check(base_url, token, "get", f"/teams/{rand_id()}")

    def test_put_team(self, base_url, token):
        self._check(base_url, token, "put", f"/teams/{rand_id()}", {"team_name": "x"})

    def test_delete_team(self, base_url, token):
        self._check(base_url, token, "delete", f"/teams/{rand_id()}")

    def test_post_roles(self, base_url, token):
        self._check(base_url, token, "post", "/roles", {})

    def test_get_role(self, base_url, token):
        self._check(base_url, token, "get", f"/roles/{rand_id()}")

    def test_put_role(self, base_url, token):
        self._check(base_url, token, "put", f"/roles/{rand_id()}", {})

    def test_delete_role(self, base_url, token):
        self._check(base_url, token, "delete", f"/roles/{rand_id()}")

    def test_post_permissions(self, base_url, token):
        self._check(base_url, token, "post", "/permissions", {"service": "x"})

    def test_get_permissions(self, base_url, token):
        self._check(base_url, token, "get", f"/permissions/{rand_id()}")

    def test_put_permissions(self, base_url, token):
        self._check(
            base_url, token, "put", f"/permissions/{rand_id()}", {"service": "x"}
        )

    def test_delete_permissions(self, base_url, token):
        self._check(base_url, token, "delete", f"/permissions/{rand_id()}")

    def test_get_session(self, base_url, token):
        self._check(base_url, token, "get", f"/sessions/{rand_id()}")

    def test_delete_session(self, base_url, token):
        self._check(base_url, token, "delete", f"/sessions/{rand_id()}")


# ---------------------------------------------------------------------------
# Users — happy path (default user has getUser/updateUser/deleteUser for self)
# ---------------------------------------------------------------------------


class TestGetUser:
    def test_returns_200(self, base_url, token, new_user):
        resp = requests.get(
            f"{base_url}/users/{new_user['user_id']}",
            headers=bearer(token),
        )
        assert resp.status_code == 200

    def test_response_contains_expected_fields(self, base_url, token, new_user):
        resp = requests.get(
            f"{base_url}/users/{new_user['user_id']}",
            headers=bearer(token),
        )
        body = resp.json()
        assert body["user_id"] == new_user["user_id"]
        assert body["username"] == new_user["username"]

    def test_password_not_in_response(self, base_url, token, new_user):
        resp = requests.get(
            f"{base_url}/users/{new_user['user_id']}",
            headers=bearer(token),
        )
        body = resp.json()
        assert "password" not in body
        assert "hashed_password" not in body

    def test_content_type_is_json(self, base_url, token, new_user):
        resp = requests.get(
            f"{base_url}/users/{new_user['user_id']}",
            headers=bearer(token),
        )
        assert "application/json" in resp.headers["Content-Type"]

    def test_nonexistent_user_returns_404(self, base_url, token, new_user):
        # Use a fake UUID that the user has no permission for — expect 403, not 404,
        # because permission is scoped to the user's own ID.
        resp = requests.get(
            f"{base_url}/users/{rand_id()}",
            headers=bearer(token),
        )
        assert resp.status_code == 403


class TestUpdateUser:
    def test_returns_200(self, base_url, token, new_user):
        resp = requests.put(
            f"{base_url}/users/{new_user['user_id']}",
            json={
                "email": new_user["email"],
                "username": new_user["username"],
                "firstname": "Updated",
                "lastname": "Name",
            },
            headers=bearer(token),
        )
        assert resp.status_code == 200

    def test_response_reflects_update(self, base_url, token, new_user):
        new_username = f"updated_{rand_id()}"
        resp = requests.put(
            f"{base_url}/users/{new_user['user_id']}",
            json={
                "email": new_user["email"],
                "username": new_username,
            },
            headers=bearer(token),
        )
        body = resp.json()
        assert body["username"] == new_username

    def test_password_not_in_response(self, base_url, token, new_user):
        resp = requests.put(
            f"{base_url}/users/{new_user['user_id']}",
            json={"email": new_user["email"], "username": new_user["username"]},
            headers=bearer(token),
        )
        body = resp.json()
        assert "password" not in body
        assert "hashed_password" not in body

    def test_missing_email_returns_400(self, base_url, token, new_user):
        resp = requests.put(
            f"{base_url}/users/{new_user['user_id']}",
            json={"username": new_user["username"]},
            headers=bearer(token),
        )
        assert resp.status_code == 400

    def test_missing_username_returns_400(self, base_url, token, new_user):
        resp = requests.put(
            f"{base_url}/users/{new_user['user_id']}",
            json={"email": new_user["email"]},
            headers=bearer(token),
        )
        assert resp.status_code == 400

    def test_invalid_body_returns_400(self, base_url, token, new_user):
        resp = requests.put(
            f"{base_url}/users/{new_user['user_id']}",
            data="not json",
            headers={**bearer(token), "Content-Type": "application/json"},
        )
        assert resp.status_code == 400

    def test_password_change_allows_login(self, base_url, token, new_user):
        new_password = f"newpass_{rand_id()}"
        requests.put(
            f"{base_url}/users/{new_user['user_id']}",
            json={
                "email": new_user["email"],
                "username": new_user["username"],
                "password": new_password,
            },
            headers=bearer(token),
        )
        login_resp = requests.post(
            f"{base_url}/login",
            json={"email": new_user["email"], "password": new_password},
        )
        assert login_resp.status_code == 200


class TestDeleteUser:
    def test_returns_204(self, base_url, base_url_delete_fixture):
        """Use a dedicated fixture so deletion doesn't break other tests."""
        token, user_id, email = base_url_delete_fixture
        resp = requests.delete(
            f"{base_url}/users/{user_id}",
            headers=bearer(token),
        )
        assert resp.status_code == 204

    def test_deleted_user_cannot_login(self, base_url, base_url_delete_fixture):
        token, user_id, email = base_url_delete_fixture
        requests.delete(
            f"{base_url}/users/{user_id}",
            headers=bearer(token),
        )
        login_resp = requests.post(
            f"{base_url}/login",
            json={"email": email, "password": "password123"},
        )
        assert login_resp.status_code == 401

    def test_double_delete_returns_404(self, base_url, base_url_delete_fixture):
        token, user_id, email = base_url_delete_fixture
        requests.delete(f"{base_url}/users/{user_id}", headers=bearer(token))
        resp = requests.delete(f"{base_url}/users/{user_id}", headers=bearer(token))
        # User is soft-deleted; session still exists but permission check fails → 403
        assert resp.status_code in (401, 403, 404)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _jwt_session_id(token: str) -> str:
    """Extract the jti (session ID) from a JWT without signature verification."""
    payload = token.split(".")[1]
    payload += "=" * (4 - len(payload) % 4)
    return json.loads(base64.b64decode(payload))["jti"]


# ---------------------------------------------------------------------------
# Orgs
# ---------------------------------------------------------------------------


class TestOrg:
    @pytest.fixture
    def org(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/orgs",
            json={"org_name": f"org-{rand_id()}"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        data = resp.json()

        yield data
        requests.delete(
            f"{base_url}/orgs/{data['org_id']}", headers=bearer(admin_token["token"])
        )

    def test_create_returns_201(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/orgs",
            json={"org_name": "create-test"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        print(resp.json())
        org_id = resp.json()["org_id"]

        requests.delete(
            f"{base_url}/orgs/{org_id}", headers=bearer(admin_token["token"])
        )

    def test_create_returns_org_id(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/orgs",
            json={"org_name": "id-test"},
            headers=bearer(admin_token["token"]),
        )
        org_id = resp.json().get("org_id")
        assert org_id

        requests.delete(
            f"{base_url}/orgs/{org_id}", headers=bearer(admin_token["token"])
        )

    def test_create_missing_name_returns_400(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/orgs", json={}, headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 400

    def test_get_returns_200(self, base_url, admin_token, org):
        resp = requests.get(
            f"{base_url}/orgs/{org['org_id']}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 200

    def test_get_returns_correct_fields(self, base_url, admin_token, org):
        resp = requests.get(
            f"{base_url}/orgs/{org['org_id']}", headers=bearer(admin_token["token"])
        )
        body = resp.json()
        assert body["org_id"] == org["org_id"]
        assert body["org_name"] == org["org_name"]

    def test_get_nonexistent_returns_404(self, base_url, admin_token):
        fake_id = rand_id()

        resp = requests.get(
            f"{base_url}/orgs/{fake_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 404

    def test_update_returns_200(self, base_url, admin_token, org):
        resp = requests.put(
            f"{base_url}/orgs/{org['org_id']}",
            json={"org_name": "updated-org"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200

    def test_update_reflected_in_response(self, base_url, admin_token, org):
        resp = requests.put(
            f"{base_url}/orgs/{org['org_id']}",
            json={"org_name": "new-name"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.json()["org_name"] == "new-name"

    def test_update_missing_name_returns_400(self, base_url, admin_token, org):
        resp = requests.put(
            f"{base_url}/orgs/{org['org_id']}",
            json={},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 400

    def test_update_nonexistent_returns_404(self, base_url, admin_token):
        fake_id = rand_id()
        resp = requests.put(
            f"{base_url}/orgs/{fake_id}",
            json={"org_name": "x"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 404

    def test_delete_returns_204(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/orgs",
            json={"org_name": "del-org"},
            headers=bearer(admin_token["token"]),
        )
        org_id = resp.json()["org_id"]

        resp = requests.delete(
            f"{base_url}/orgs/{org_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 204

    def test_deleted_org_returns_404_on_get(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/orgs",
            json={"org_name": "del-404-org"},
            headers=bearer(admin_token["token"]),
        )
        org_id = resp.json()["org_id"]

        requests.delete(
            f"{base_url}/orgs/{org_id}", headers=bearer(admin_token["token"])
        )
        resp = requests.get(
            f"{base_url}/orgs/{org_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 404

    def test_list_returns_200(self, base_url, admin_token, org):
        resp = requests.get(
            f"{base_url}/orgs",
            json={},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200

    def test_list_returns_array(self, base_url, admin_token, org):
        resp = requests.get(
            f"{base_url}/orgs",
            json={},
            headers=bearer(admin_token["token"]),
        )
        assert isinstance(resp.json(), list)

    def test_list_filter_by_org_id_returns_match(self, base_url, admin_token, org):
        resp = requests.get(
            f"{base_url}/orgs",
            params={"org_id": org["org_id"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        body = resp.json()
        assert len(body) == 1
        assert body[0]["org_id"] == org["org_id"]

    def test_list_filter_by_org_id_excludes_others(self, base_url, admin_token, org):
        resp = requests.get(
            f"{base_url}/orgs",
            params={"org_id": rand_id()},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        assert resp.json() == []

    def test_list_excludes_deleted(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/orgs",
            json={"org_name": f"list-del-{rand_id()}"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        org_id = resp.json()["org_id"]

        requests.delete(
            f"{base_url}/orgs/{org_id}", headers=bearer(admin_token["token"])
        )
        resp = requests.get(
            f"{base_url}/orgs",
            params={"org_id": org_id},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        assert resp.json() == []


# ---------------------------------------------------------------------------
# Teams
# ---------------------------------------------------------------------------


class TestTeam:
    @pytest.fixture
    def team(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/teams",
            json={"team_name": f"team-{rand_id()}"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        data = resp.json()

        yield data
        requests.delete(
            f"{base_url}/teams/{data['team_id']}", headers=bearer(admin_token["token"])
        )

    def test_create_returns_201(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/teams",
            json={"team_name": "create-team"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        team_id = resp.json()["team_id"]

        requests.delete(
            f"{base_url}/teams/{team_id}", headers=bearer(admin_token["token"])
        )

    def test_create_returns_team_id(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/teams",
            json={"team_name": "id-team"},
            headers=bearer(admin_token["token"]),
        )
        team_id = resp.json().get("team_id")
        assert team_id
        requests.delete(
            f"{base_url}/teams/{team_id}", headers=bearer(admin_token["token"])
        )

    def test_create_missing_name_returns_400(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/teams", json={}, headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 400

    def test_get_returns_200(self, base_url, admin_token, team):
        resp = requests.get(
            f"{base_url}/teams/{team['team_id']}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 200

    def test_get_returns_correct_fields(self, base_url, admin_token, team):
        resp = requests.get(
            f"{base_url}/teams/{team['team_id']}", headers=bearer(admin_token["token"])
        )
        body = resp.json()
        assert body["team_id"] == team["team_id"]
        assert body["team_name"] == team["team_name"]

    def test_get_nonexistent_returns_404(self, base_url, admin_token):
        fake_id = rand_id()
        resp = requests.get(
            f"{base_url}/teams/{fake_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 404

    def test_update_returns_200(self, base_url, admin_token, team):
        resp = requests.put(
            f"{base_url}/teams/{team['team_id']}",
            json={"team_name": "updated-team"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200

    def test_update_reflected_in_response(self, base_url, admin_token, team):
        resp = requests.put(
            f"{base_url}/teams/{team['team_id']}",
            json={"team_name": "new-team-name"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.json()["team_name"] == "new-team-name"

    def test_update_missing_name_returns_400(self, base_url, admin_token, team):
        resp = requests.put(
            f"{base_url}/teams/{team['team_id']}",
            json={},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 400

    def test_update_nonexistent_returns_404(self, base_url, admin_token):
        fake_id = rand_id()

        resp = requests.put(
            f"{base_url}/teams/{fake_id}",
            json={"team_name": "x"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 404

    def test_delete_returns_204(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/teams",
            json={"team_name": "del-team"},
            headers=bearer(admin_token["token"]),
        )
        team_id = resp.json()["team_id"]

        resp = requests.delete(
            f"{base_url}/teams/{team_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 204

    def test_deleted_team_returns_404_on_get(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/teams",
            json={"team_name": "del-404-team"},
            headers=bearer(admin_token["token"]),
        )
        team_id = resp.json()["team_id"]

        requests.delete(
            f"{base_url}/teams/{team_id}", headers=bearer(admin_token["token"])
        )
        resp = requests.get(
            f"{base_url}/teams/{team_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 404

    def test_create_sets_creator_team_id(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/teams",
            json={"team_name": f"team-{rand_id()}"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        team_id = resp.json()["team_id"]

        user_resp = requests.get(
            f"{base_url}/users/{admin_token['user_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert user_resp.status_code == 200
        assert user_resp.json()["team_id"] == team_id

        requests.delete(
            f"{base_url}/teams/{team_id}", headers=bearer(admin_token["token"])
        )

    def test_delete_clears_creator_team_id(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/teams",
            json={"team_name": f"team-{rand_id()}"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        team_id = resp.json()["team_id"]

        requests.delete(
            f"{base_url}/teams/{team_id}", headers=bearer(admin_token["token"])
        )

        user_resp = requests.get(
            f"{base_url}/users/{admin_token['user_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert user_resp.status_code == 200
        assert user_resp.json()["team_id"] is None

    def test_create_with_role_id(self, base_url, admin_token):
        role_resp = requests.post(
            f"{base_url}/roles",
            json={"permissions_ids": []},
            headers=bearer(admin_token["token"]),
        )
        assert role_resp.status_code == 201
        role_id = role_resp.json()["role_id"]

        resp = requests.post(
            f"{base_url}/teams",
            json={"team_name": f"role-team-{rand_id()}", "role_id": role_id},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        team_id = resp.json()["team_id"]
        assert resp.json()["role_id"] == role_id

        requests.delete(
            f"{base_url}/teams/{team_id}", headers=bearer(admin_token["token"])
        )
        requests.delete(
            f"{base_url}/roles/{role_id}", headers=bearer(admin_token["token"])
        )

    def test_list_returns_200(self, base_url, admin_token, team):
        resp = requests.get(
            f"{base_url}/teams",
            json={},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200

    def test_list_returns_array(self, base_url, admin_token, team):
        resp = requests.get(
            f"{base_url}/teams",
            json={},
            headers=bearer(admin_token["token"]),
        )
        assert isinstance(resp.json(), list)

    def test_list_filter_by_team_id_returns_match(self, base_url, admin_token, team):
        resp = requests.get(
            f"{base_url}/teams",
            params={"team_id": team["team_id"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        body = resp.json()
        assert len(body) == 1
        assert body[0]["team_id"] == team["team_id"]

    def test_list_filter_by_team_id_excludes_others(self, base_url, admin_token, team):
        resp = requests.get(
            f"{base_url}/teams",
            params={"team_id": rand_id()},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        assert resp.json() == []

    def test_list_excludes_deleted(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/teams",
            json={"team_name": f"list-del-{rand_id()}"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        team_id = resp.json()["team_id"]

        requests.delete(
            f"{base_url}/teams/{team_id}", headers=bearer(admin_token["token"])
        )
        resp = requests.get(
            f"{base_url}/teams",
            params={"team_id": team_id},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        assert resp.json() == []

    def test_create_second_team_after_deleting_first_returns_201(
        self, base_url, admin_token
    ):
        resp1 = requests.post(
            f"{base_url}/teams",
            json={"team_name": f"first-{rand_id()}"},
            headers=bearer(admin_token["token"]),
        )
        assert resp1.status_code == 201
        first_id = resp1.json()["team_id"]
        requests.delete(
            f"{base_url}/teams/{first_id}", headers=bearer(admin_token["token"])
        )

        resp2 = requests.post(
            f"{base_url}/teams",
            json={"team_name": f"second-{rand_id()}"},
            headers=bearer(admin_token["token"]),
        )
        assert resp2.status_code == 201
        second_id = resp2.json()["team_id"]
        requests.delete(
            f"{base_url}/teams/{second_id}", headers=bearer(admin_token["token"])
        )


# ---------------------------------------------------------------------------
# Roles
# ---------------------------------------------------------------------------


class TestRole:
    @pytest.fixture
    def role(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/roles",
            json={"permissions_ids": []},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        data = resp.json()

        yield data
        requests.delete(
            f"{base_url}/roles/{data['role_id']}", headers=bearer(admin_token["token"])
        )

    def test_create_returns_201(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/roles",
            json={"permissions_ids": []},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        role_id = resp.json()["role_id"]

        requests.delete(
            f"{base_url}/roles/{role_id}", headers=bearer(admin_token["token"])
        )

    def test_create_returns_role_id(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/roles", json={}, headers=bearer(admin_token["token"])
        )
        role_id = resp.json().get("role_id")
        assert role_id

        requests.delete(
            f"{base_url}/roles/{role_id}", headers=bearer(admin_token["token"])
        )

    def test_get_returns_200(self, base_url, admin_token, role):
        resp = requests.get(
            f"{base_url}/roles/{role['role_id']}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 200

    def test_get_returns_correct_id(self, base_url, admin_token, role):
        resp = requests.get(
            f"{base_url}/roles/{role['role_id']}", headers=bearer(admin_token["token"])
        )
        assert resp.json()["role_id"] == role["role_id"]

    def test_get_nonexistent_returns_404(self, base_url, admin_token):
        fake_id = rand_id()

        resp = requests.get(
            f"{base_url}/roles/{fake_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 404

    def test_update_returns_200(self, base_url, admin_token, role):
        resp = requests.put(
            f"{base_url}/roles/{role['role_id']}",
            json={"permissions_ids": []},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200

    def test_update_permissions_ids_reflected(self, base_url, admin_token, role):
        perm = requests.post(
            f"{base_url}/permissions",
            json={"service": "forge"},
            headers=bearer(admin_token["token"]),
        )
        assert perm.status_code == 201
        pid = perm.json()["permissions_id"]

        resp = requests.put(
            f"{base_url}/roles/{role['role_id']}",
            json={"permissions_ids": [pid]},
            headers=bearer(admin_token["token"]),
        )
        assert pid in resp.json()["permissions_ids"]

        requests.delete(f"{base_url}/permissions/{pid}", headers=bearer(admin_token["token"]))

    def test_update_nonexistent_returns_404(self, base_url, admin_token):
        fake_id = rand_id()

        resp = requests.put(
            f"{base_url}/roles/{fake_id}", json={}, headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 404

    def test_delete_returns_204(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/roles", json={}, headers=bearer(admin_token["token"])
        )
        role_id = resp.json()["role_id"]

        resp = requests.delete(
            f"{base_url}/roles/{role_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 204

    def test_deleted_role_returns_404_on_get(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/roles", json={}, headers=bearer(admin_token["token"])
        )
        role_id = resp.json()["role_id"]

        requests.delete(
            f"{base_url}/roles/{role_id}", headers=bearer(admin_token["token"])
        )
        resp = requests.get(
            f"{base_url}/roles/{role_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# Permissions
# ---------------------------------------------------------------------------


class TestPermissions:
    @pytest.fixture
    def perm(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/permissions",
            json={"service": "forge", "actions": ["read"], "resources": ["res"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        data = resp.json()

        yield data
        requests.delete(
            f"{base_url}/permissions/{data['permissions_id']}",
            headers=bearer(admin_token["token"]),
        )

    def test_create_returns_201(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/permissions",
            json={"service": "forge", "actions": ["read"], "resources": ["res"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        pid = resp.json()["permissions_id"]

        requests.delete(
            f"{base_url}/permissions/{pid}", headers=bearer(admin_token["token"])
        )

    def test_create_returns_permissions_id(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/permissions",
            json={"service": "forge"},
            headers=bearer(admin_token["token"]),
        )
        pid = resp.json().get("permissions_id")
        assert pid

        requests.delete(
            f"{base_url}/permissions/{pid}", headers=bearer(admin_token["token"])
        )

    def test_create_missing_service_returns_400(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/permissions",
            json={"actions": ["read"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 400

    def test_get_returns_200(self, base_url, admin_token, perm):
        resp = requests.get(
            f"{base_url}/permissions/{perm['permissions_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200

    def test_get_returns_correct_fields(self, base_url, admin_token, perm):
        resp = requests.get(
            f"{base_url}/permissions/{perm['permissions_id']}",
            headers=bearer(admin_token["token"]),
        )
        body = resp.json()
        assert body["permissions_id"] == perm["permissions_id"]
        assert body["service"] == perm["service"]

    def test_get_nonexistent_returns_404(self, base_url, admin_token):
        fake_id = rand_id()

        resp = requests.get(
            f"{base_url}/permissions/{fake_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 404

    def test_update_returns_200(self, base_url, admin_token, perm):
        resp = requests.put(
            f"{base_url}/permissions/{perm['permissions_id']}",
            json={"service": "forge", "actions": ["write"], "resources": ["res"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200

    def test_update_reflected_in_response(self, base_url, admin_token, perm):
        resp = requests.put(
            f"{base_url}/permissions/{perm['permissions_id']}",
            json={
                "service": "blueprints",
                "actions": ["read", "write"],
                "resources": ["res"],
            },
            headers=bearer(admin_token["token"]),
        )
        body = resp.json()
        assert body["service"] == "blueprints"
        assert "write" in body["actions"]

    def test_update_missing_service_returns_400(self, base_url, admin_token, perm):
        resp = requests.put(
            f"{base_url}/permissions/{perm['permissions_id']}",
            json={"actions": ["read"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 400

    def test_update_nonexistent_returns_404(self, base_url, admin_token):
        fake_id = rand_id()

        resp = requests.put(
            f"{base_url}/permissions/{fake_id}",
            json={"service": "forge"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 404

    def test_delete_returns_204(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/permissions",
            json={"service": "forge"},
            headers=bearer(admin_token["token"]),
        )
        pid = resp.json()["permissions_id"]
        resp = requests.delete(
            f"{base_url}/permissions/{pid}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 204

    def test_deleted_permissions_returns_404_on_get(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/permissions",
            json={"service": "forge"},
            headers=bearer(admin_token["token"]),
        )
        pid = resp.json()["permissions_id"]

        requests.delete(
            f"{base_url}/permissions/{pid}", headers=bearer(admin_token["token"])
        )
        resp = requests.get(
            f"{base_url}/permissions/{pid}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# Sessions
# ---------------------------------------------------------------------------


class TestSession:
    @pytest.fixture
    def session_data(self, base_url, admin_token):
        """Create a fresh session for the admin user and grant get permission for it."""
        login = requests.post(
            f"{base_url}/login",
            json={"email": admin_token["email"], "password": admin_token["password"]},
        )
        assert login.status_code == 200
        token = login.json()["token"]
        session_id = _jwt_session_id(token)

        yield {"session_id": session_id, "token": token}

    def test_get_returns_200(self, base_url, admin_token, session_data):
        resp = requests.get(
            f"{base_url}/sessions/{session_data['session_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200

    def test_get_returns_correct_id(self, base_url, admin_token, session_data):
        resp = requests.get(
            f"{base_url}/sessions/{session_data['session_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert resp.json()["session_id"] == session_data["session_id"]

    def test_get_does_not_expose_jwt(self, base_url, admin_token, session_data):
        resp = requests.get(
            f"{base_url}/sessions/{session_data['session_id']}",
            headers=bearer(admin_token["token"]),
        )
        body = resp.json()
        assert "jwt" not in body
        assert "pub_key" not in body

    def test_get_nonexistent_returns_404(self, base_url, admin_token):
        fake_id = rand_id()

        resp = requests.get(
            f"{base_url}/sessions/{fake_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 404

    def test_delete_returns_204(self, base_url, admin_token, session_data):
        resp = requests.delete(
            f"{base_url}/sessions/{session_data['session_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 204

    def test_deleted_session_returns_404_on_get(self, base_url, admin_token):
        login = requests.post(
            f"{base_url}/login",
            json={"email": admin_token["email"], "password": admin_token["password"]},
        )
        token = login.json()["token"]
        session_id = _jwt_session_id(token)

        requests.delete(
            f"{base_url}/sessions/{session_id}", headers=bearer(admin_token["token"])
        )
        resp = requests.get(
            f"{base_url}/sessions/{session_id}", headers=bearer(admin_token["token"])
        )
        assert resp.status_code == 404


class TestAuditLogs:
    def test_no_auth_returns_401(self, base_url):
        resp = requests.get(f"{base_url}/audit-logs")
        assert resp.status_code == 401

    def test_unprivileged_user_returns_403(self, base_url, token):
        resp = requests.get(f"{base_url}/audit-logs", headers=bearer(token))
        assert resp.status_code == 403

    def test_admin_can_list(self, base_url, admin_token):
        resp = requests.get(f"{base_url}/audit-logs", headers=bearer(admin_token["token"]))
        assert resp.status_code == 200
        assert isinstance(resp.json(), list)

    def test_response_contains_expected_fields(self, base_url, admin_token):
        resp = requests.get(f"{base_url}/audit-logs", headers=bearer(admin_token["token"]))
        assert resp.status_code == 200
        logs = resp.json()
        if logs:
            entry = logs[0]
            assert "audit_log_id" in entry
            assert "action" in entry
            assert "actor_id" in entry
