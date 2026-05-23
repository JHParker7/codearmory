import uuid
import requests


def unique_payload(**overrides):
    """Return a valid signup payload with unique email and username."""
    uid = uuid.uuid4().hex[:8]
    payload = {
        "email": f"test_{uid}@example.com",
        "username": f"user_{uid}",
        "password": "password123",
        "firstname": "Test",
        "lastname": "User",
    }
    payload.update(overrides)
    return payload


class TestSignupSuccess:
    def test_returns_201(self, base_url):
        resp = requests.post(f"{base_url}/signup", json=unique_payload())
        assert resp.status_code == 201

    def test_response_contains_user_id(self, base_url):
        resp = requests.post(f"{base_url}/signup", json=unique_payload())
        assert "user_id" in resp.json()
        assert resp.json()["user_id"] != ""

    def test_response_reflects_submitted_fields(self, base_url):
        payload = unique_payload()
        body = requests.post(f"{base_url}/signup", json=payload).json()
        assert body["email"] == payload["email"]
        assert body["username"] == payload["username"]
        assert body["firstname"] == payload["firstname"]
        assert body["lastname"] == payload["lastname"]

    def test_password_not_in_response(self, base_url):
        resp = requests.post(f"{base_url}/signup", json=unique_payload())
        body = resp.json()
        assert "password" not in body
        assert "hashed_password" not in body

    def test_optional_name_fields_default_to_empty(self, base_url):
        payload = unique_payload()
        del payload["firstname"]
        del payload["lastname"]
        body = requests.post(f"{base_url}/signup", json=payload).json()
        assert body["firstname"] == ""
        assert body["lastname"] == ""

    def test_content_type_is_json(self, base_url):
        resp = requests.post(f"{base_url}/signup", json=unique_payload())
        assert "application/json" in resp.headers["Content-Type"]


class TestSignupValidation:
    def test_missing_email_returns_400(self, base_url):
        payload = unique_payload()
        del payload["email"]
        resp = requests.post(f"{base_url}/signup", json=payload)
        assert resp.status_code == 400

    def test_missing_username_returns_400(self, base_url):
        payload = unique_payload()
        del payload["username"]
        resp = requests.post(f"{base_url}/signup", json=payload)
        assert resp.status_code == 400

    def test_missing_password_returns_400(self, base_url):
        payload = unique_payload()
        del payload["password"]
        resp = requests.post(f"{base_url}/signup", json=payload)
        assert resp.status_code == 400

    def test_empty_email_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/signup", json=unique_payload(email=""))
        assert resp.status_code == 400

    def test_empty_username_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/signup", json=unique_payload(username=""))
        assert resp.status_code == 400

    def test_empty_password_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/signup", json=unique_payload(password=""))
        assert resp.status_code == 400

    def test_invalid_json_returns_400(self, base_url):
        resp = requests.post(
            f"{base_url}/signup",
            data="not json",
            headers={"Content-Type": "application/json"},
        )
        assert resp.status_code == 400

    def test_empty_body_returns_400(self, base_url):
        resp = requests.post(f"{base_url}/signup", json={})
        assert resp.status_code == 400


class TestSignupConflicts:
    def test_duplicate_email_returns_409(self, base_url, new_user):
        payload = unique_payload(email=new_user["email"])
        resp = requests.post(f"{base_url}/signup", json=payload)
        assert resp.status_code == 409

    def test_duplicate_username_returns_409(self, base_url, new_user):
        payload = unique_payload(username=new_user["username"])
        resp = requests.post(f"{base_url}/signup", json=payload)
        assert resp.status_code == 409
