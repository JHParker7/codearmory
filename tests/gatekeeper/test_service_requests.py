"""Integration tests for the service permission request lifecycle.

Service accounts (blueprints, forge) are pre-seeded by gatekeeper at startup
from GATEKEEPER_SERVICES. Tests use the blueprints service account with its
well-known local key.

POST /service-permission-requests requires X-Service-Key: name:key (service auth).
GET/approve/decline /service-permission-requests require a user JWT with
the appropriate gatekeeper permissions (admin_token has wildcard access).
"""

import uuid
import pytest
import requests


_SERVICE_NAME = "blueprints"
_SERVICE_PLAIN_KEY = "blueprints-local-secret"
_SERVICE_KEY_HEADER = f"{_SERVICE_NAME}:{_SERVICE_PLAIN_KEY}"


def bearer(token):
    return {"Authorization": f"Bearer {token}"}


def svc_header():
    return {"X-Service-Key": _SERVICE_KEY_HEADER}


def rand_id():
    return uuid.uuid4().hex[:8]


def _create_spr(base_url, prefix=""):
    name = f"{prefix or 'integ'}-{rand_id()}"
    resp = requests.post(
        f"{base_url}/service-permission-requests",
        json={
            "name": name,
            "service": _SERVICE_NAME,
            "actions": ["read"],
            "resources": ["blueprints/states"],
        },
        headers=svc_header(),
    )
    assert resp.status_code == 201, f"create SPR failed: {resp.text}"
    return resp.json()


# ---------------------------------------------------------------------------
# TestCreateServicePermissionRequest
# ---------------------------------------------------------------------------


class TestCreateServicePermissionRequest:
    def test_no_service_key_returns_401(self, base_url):
        resp = requests.post(
            f"{base_url}/service-permission-requests",
            json={"name": "p", "service": _SERVICE_NAME, "actions": ["read"], "resources": ["r"]},
        )
        assert resp.status_code == 401

    def test_wrong_key_returns_401(self, base_url):
        resp = requests.post(
            f"{base_url}/service-permission-requests",
            json={"name": "p", "service": _SERVICE_NAME, "actions": ["read"], "resources": ["r"]},
            headers={"X-Service-Key": f"{_SERVICE_NAME}:wrong-key"},
        )
        assert resp.status_code == 401

    def test_missing_fields_returns_400(self, base_url):
        resp = requests.post(
            f"{base_url}/service-permission-requests",
            json={"name": "p"},
            headers=svc_header(),
        )
        assert resp.status_code == 400

    def test_empty_actions_returns_400(self, base_url):
        resp = requests.post(
            f"{base_url}/service-permission-requests",
            json={"name": "p", "service": _SERVICE_NAME, "actions": [], "resources": ["r"]},
            headers=svc_header(),
        )
        assert resp.status_code == 400

    def test_success_returns_201(self, base_url):
        resp = requests.post(
            f"{base_url}/service-permission-requests",
            json={
                "name": f"create-{rand_id()}",
                "service": _SERVICE_NAME,
                "actions": ["read"],
                "resources": ["blueprints/states"],
            },
            headers=svc_header(),
        )
        assert resp.status_code == 201
        body = resp.json()
        assert "request_id" in body
        assert body["status"] == "pending"

    def test_duplicate_name_returns_409(self, base_url):
        name = f"dup-{rand_id()}"
        payload = {
            "name": name,
            "service": _SERVICE_NAME,
            "actions": ["read"],
            "resources": ["blueprints/states"],
        }
        r1 = requests.post(f"{base_url}/service-permission-requests", json=payload, headers=svc_header())
        assert r1.status_code == 201
        r2 = requests.post(f"{base_url}/service-permission-requests", json=payload, headers=svc_header())
        assert r2.status_code == 409


# ---------------------------------------------------------------------------
# TestListServicePermissionRequests
# ---------------------------------------------------------------------------


class TestListServicePermissionRequests:
    @pytest.fixture
    def spr(self, base_url):
        return _create_spr(base_url, "list")

    def test_no_auth_returns_401(self, base_url):
        resp = requests.get(f"{base_url}/service-permission-requests")
        assert resp.status_code == 401

    def test_unprivileged_user_returns_403(self, base_url, token):
        resp = requests.get(
            f"{base_url}/service-permission-requests",
            headers=bearer(token),
        )
        assert resp.status_code == 403

    def test_admin_can_list(self, base_url, admin_token, spr):
        resp = requests.get(
            f"{base_url}/service-permission-requests",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        assert isinstance(resp.json(), list)

    def test_created_spr_appears_in_list(self, base_url, admin_token, spr):
        resp = requests.get(
            f"{base_url}/service-permission-requests",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        ids = [r["request_id"] for r in resp.json()]
        assert spr["request_id"] in ids

    def test_filter_by_status_pending(self, base_url, admin_token, spr):
        resp = requests.get(
            f"{base_url}/service-permission-requests",
            params={"status": "pending"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        for r in resp.json():
            assert r["status"] == "pending"


# ---------------------------------------------------------------------------
# TestGetServicePermissionRequest
# ---------------------------------------------------------------------------


class TestGetServicePermissionRequest:
    @pytest.fixture
    def spr(self, base_url):
        return _create_spr(base_url, "get")

    def test_no_auth_returns_401(self, base_url, spr):
        resp = requests.get(f"{base_url}/service-permission-requests/{spr['request_id']}")
        assert resp.status_code == 401

    def test_unprivileged_user_returns_403(self, base_url, token, spr):
        resp = requests.get(
            f"{base_url}/service-permission-requests/{spr['request_id']}",
            headers=bearer(token),
        )
        assert resp.status_code == 403

    def test_admin_can_get(self, base_url, admin_token, spr):
        resp = requests.get(
            f"{base_url}/service-permission-requests/{spr['request_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        assert resp.json()["request_id"] == spr["request_id"]

    def test_nonexistent_returns_404(self, base_url, admin_token):
        resp = requests.get(
            f"{base_url}/service-permission-requests/{uuid.uuid4().hex}",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# TestApproveServicePermissionRequest
# ---------------------------------------------------------------------------


class TestApproveServicePermissionRequest:
    @pytest.fixture
    def spr(self, base_url):
        return _create_spr(base_url, "approve")

    def test_no_auth_returns_401(self, base_url, spr):
        resp = requests.post(f"{base_url}/service-permission-requests/{spr['request_id']}/approve")
        assert resp.status_code == 401

    def test_unprivileged_user_returns_403(self, base_url, token, spr):
        resp = requests.post(
            f"{base_url}/service-permission-requests/{spr['request_id']}/approve",
            headers=bearer(token),
        )
        assert resp.status_code == 403

    def test_admin_can_approve(self, base_url, admin_token, spr):
        resp = requests.post(
            f"{base_url}/service-permission-requests/{spr['request_id']}/approve",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 204

    def test_approve_sets_status(self, base_url, admin_token, spr):
        requests.post(
            f"{base_url}/service-permission-requests/{spr['request_id']}/approve",
            headers=bearer(admin_token["token"]),
        )
        resp = requests.get(
            f"{base_url}/service-permission-requests/{spr['request_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        assert resp.json()["status"] == "approved"

    def test_nonexistent_returns_404(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/service-permission-requests/{uuid.uuid4().hex}/approve",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# TestDeclineServicePermissionRequest
# ---------------------------------------------------------------------------


class TestDeclineServicePermissionRequest:
    @pytest.fixture
    def spr(self, base_url):
        return _create_spr(base_url, "decline")

    def test_no_auth_returns_401(self, base_url, spr):
        resp = requests.post(f"{base_url}/service-permission-requests/{spr['request_id']}/decline")
        assert resp.status_code == 401

    def test_unprivileged_user_returns_403(self, base_url, token, spr):
        resp = requests.post(
            f"{base_url}/service-permission-requests/{spr['request_id']}/decline",
            headers=bearer(token),
        )
        assert resp.status_code == 403

    def test_admin_can_decline(self, base_url, admin_token, spr):
        resp = requests.post(
            f"{base_url}/service-permission-requests/{spr['request_id']}/decline",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 204

    def test_decline_sets_status(self, base_url, admin_token, spr):
        requests.post(
            f"{base_url}/service-permission-requests/{spr['request_id']}/decline",
            headers=bearer(admin_token["token"]),
        )
        resp = requests.get(
            f"{base_url}/service-permission-requests/{spr['request_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        assert resp.json()["status"] == "declined"

    def test_double_decline_returns_409(self, base_url, admin_token, spr):
        requests.post(
            f"{base_url}/service-permission-requests/{spr['request_id']}/decline",
            headers=bearer(admin_token["token"]),
        )
        resp = requests.post(
            f"{base_url}/service-permission-requests/{spr['request_id']}/decline",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 409

    def test_nonexistent_returns_404(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/service-permission-requests/{uuid.uuid4().hex}/decline",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 404
