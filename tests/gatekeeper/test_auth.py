import uuid
import requests


class TestLoginSuccess:
    def test_returns_200(self, base_url, new_user):
        resp = requests.post(
            f"{base_url}/login",
            json={
                "email": new_user["email"],
                "password": new_user["password"],
            },
        )
        assert resp.status_code == 200

    def test_response_contains_token(self, base_url, new_user):
        resp = requests.post(
            f"{base_url}/login",
            json={
                "email": new_user["email"],
                "password": new_user["password"],
            },
        )
        body = resp.json()
        assert "token" in body
        assert body["token"] != ""

    def test_token_is_jwt_format(self, base_url, new_user):
        resp = requests.post(
            f"{base_url}/login",
            json={
                "email": new_user["email"],
                "password": new_user["password"],
            },
        )
        token = resp.json()["token"]
        assert len(token.split(".")) == 3

    def test_content_type_is_json(self, base_url, new_user):
        resp = requests.post(
            f"{base_url}/login",
            json={
                "email": new_user["email"],
                "password": new_user["password"],
            },
        )
        assert "application/json" in resp.headers["Content-Type"]


class TestLoginValidation:
    def test_missing_email_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/login", json={"password": "password123"})
        assert resp.status_code == 400

    def test_missing_password_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/login", json={"email": "x@example.com"})
        assert resp.status_code == 400

    def test_empty_body_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/login", json={})
        assert resp.status_code == 400

    def test_invalid_json_returns_400(self, base_url):
        resp = requests.post(
            f"{base_url}/login",
            data="not json",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status_code == 400


class TestLoginInvalidCredentials:
    def test_wrong_password_returns_401(self, base_url, new_user):
        resp = requests.post(
            f"{base_url}/login",
            json={
                "email": new_user["email"],
                "password": "wrongpassword",
            },
        )
        assert resp.status_code == 401

    def test_unknown_email_returns_401(self, base_url):
        resp = requests.post(
            f"{base_url}/login",
            json={
                "email": f"nobody_{uuid.uuid4().hex}@example.com",
                "password": "password123",
            },
        )
        assert resp.status_code == 401

    def test_error_message_does_not_distinguish_user_vs_password(
        self, base_url, new_user
    ):
        wrong_user = requests.post(
            f"{base_url}/login",
            json={
                "email": f"nobody_{uuid.uuid4().hex}@example.com",
                "password": "password123",
            },
        )
        wrong_pass = requests.post(
            f"{base_url}/login",
            json={
                "email": new_user["email"],
                "password": "wrongpassword",
            },
        )
        assert wrong_user.text == wrong_pass.text


class TestAuthMiddleware:
    def test_no_authorization_header_returns_401(self, base_url):
        resp = requests.post(
            f"{base_url}/check_permissions",
            json={"service": "svc", "resource": "res", "action": "act"},
        )
        assert resp.status_code == 401

    def test_malformed_bearer_returns_401(self, base_url):
        resp = requests.post(
            f"{base_url}/check_permissions",
            json={"service": "svc", "resource": "res", "action": "act"},
            headers={"Authorization": "notbearer"},
        )
        assert resp.status_code == 401

    def test_invalid_token_returns_401(self, base_url):
        resp = requests.post(
            f"{base_url}/check_permissions",
            json={"service": "svc", "resource": "res", "action": "act"},
            headers={"Authorization": "Bearer invalidtoken"},
        )
        assert resp.status_code == 401

    def test_valid_token_returns_200(self, base_url, token):
        resp = requests.post(
            f"{base_url}/check_permissions",
            json={"service": "svc", "resource": "res", "action": "act"},
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 200

    def test_response_contains_authorized_field(self, base_url, token):
        resp = requests.post(
            f"{base_url}/check_permissions",
            json={"service": "svc", "resource": "res", "action": "act"},
            headers={"Authorization": f"Bearer {token}"},
        )
        body = resp.json()
        assert "authorized" in body


class TestCheckPermissions:
    def test_invalid_body_returns_400(self, base_url, token):
        resp = requests.post(
            f"{base_url}/check_permissions",
            data="not json",
            headers={
                "Authorization": f"Bearer {token}",
                "Content-Type": "application/json",
            },
        )
        assert resp.status_code == 400

    def test_nonmatching_service_returns_authorized_false(self, base_url, token):
        resp = requests.post(
            f"{base_url}/check_permissions",
            json={"service": "unknown", "resource": "unknown", "action": "unknown"},
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 200
        assert resp.json()["authorized"] is False

    def test_matching_permission_returns_authorized_true(
        self, base_url, token, new_user
    ):
        resp = requests.post(
            f"{base_url}/check_permissions",
            json={
                "service": "gatekeeper",
                "resource": f"gatekeeper/users/{new_user['user_id']}",
                "action": "getUser",
            },
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 200
        assert resp.json()["authorized"] is True

    def test_content_type_is_json(self, base_url, token):
        resp = requests.post(
            f"{base_url}/check_permissions",
            json={"service": "svc", "resource": "res", "action": "act"},
            headers={"Authorization": f"Bearer {token}"},
        )
        assert "application/json" in resp.headers["Content-Type"]
