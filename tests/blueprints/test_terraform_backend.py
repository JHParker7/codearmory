"""Integration tests for Terraform HTTP backend endpoints."""
import base64
import json
import uuid
import os

import pytest
import requests

# blueprints listens on :8093 and gatekeeper on :8081, but neither service
# publishes a host port in infra/local/compose.yml — they are only reachable on
# the compose network. CI runs this suite as the `blueprints-integration-tests`
# compose service, which sets BLUEPRINTS_URL=http://blueprints:8093 and
# GATEKEEPER_URL=http://gatekeeper:8081 via env. The defaults below mirror those
# service ports; running locally outside the compose network requires setting
# these env vars (or publishing the ports yourself).
BLUEPRINTS_URL = os.getenv("BLUEPRINTS_URL", "http://localhost:8093")
GATEKEEPER_URL = os.getenv("GATEKEEPER_URL", "http://localhost:8081")

EMAIL = "tf_backend_test@example.com"
PASSWORD = "tf_backend_pass"
USERNAME = "tf_backend_tester"

OTHER_EMAIL = "tf_backend_other@example.com"
OTHER_PASSWORD = "tf_backend_other_pass"
OTHER_USERNAME = "tf_backend_other_tester"


@pytest.fixture(scope="module")
def token():
    requests.post(
        f"{GATEKEEPER_URL}/signup",
        json={"email": EMAIL, "username": USERNAME, "password": PASSWORD},
    )
    res = requests.post(
        f"{GATEKEEPER_URL}/login",
        json={"email": EMAIL, "password": PASSWORD},
    )
    return res.json()["token"]


@pytest.fixture(scope="module")
def other_token():
    requests.post(
        f"{GATEKEEPER_URL}/signup",
        json={"email": OTHER_EMAIL, "username": OTHER_USERNAME, "password": OTHER_PASSWORD},
    )
    res = requests.post(
        f"{GATEKEEPER_URL}/login",
        json={"email": OTHER_EMAIL, "password": OTHER_PASSWORD},
    )
    return res.json()["token"]


@pytest.fixture
def workspace():
    return f"ws-blueprints-integration-tests-{uuid.uuid4().hex[:8]}"


@pytest.fixture
def bearer(token):
    return {"Authorization": f"Bearer {token}"}


@pytest.fixture
def other_bearer(other_token):
    return {"Authorization": f"Bearer {other_token}"}


@pytest.fixture
def basic_auth():
    creds = base64.b64encode(f"{EMAIL}:{PASSWORD}".encode()).decode()
    return {"Authorization": f"Basic {creds}"}


def lock_body():
    return json.dumps({
        "ID": str(uuid.uuid4()),
        "Operation": "OperationTypePlan",
        "Who": "tester@host",
        "Info": "",
        "Version": "1.5.0",
        "Created": "2024-01-01T00:00:00.000000000Z",
        "Path": "",
    })


STATE = json.dumps({
    "version": 4,
    "terraform_version": "1.5.0",
    "serial": 1,
    "lineage": str(uuid.uuid4()),
    "outputs": {},
    "resources": [],
})


# ── User-scoped routes ────────────────────────────────────────────────────────

def test_unauthenticated_request_returns_401(workspace):
    res = requests.get(f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}")
    assert res.status_code == 401


def test_get_empty_state_returns_204(bearer, workspace):
    res = requests.get(f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer)
    assert res.status_code == 204


def test_state_lifecycle(bearer, workspace):
    res = requests.get(f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer)
    assert res.status_code == 204

    res = requests.post(f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer, data=STATE)
    assert res.status_code == 200

    res = requests.get(f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer)
    assert res.status_code == 200
    assert res.json()["version"] == 4

    res = requests.delete(f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer)
    assert res.status_code == 200

    res = requests.get(f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer)
    assert res.status_code == 204


def test_lock_unlock_cycle(bearer, workspace):
    lock = lock_body()
    lock_data = json.loads(lock)

    res = requests.request("LOCK", f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer, data=lock)
    assert res.status_code == 200

    res = requests.request("LOCK", f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer, data=lock)
    assert res.status_code == 423
    assert res.json()["ID"] == lock_data["ID"]

    res = requests.request("UNLOCK", f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer, data=lock)
    assert res.status_code == 200

    res = requests.request("LOCK", f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer, data=lock)
    assert res.status_code == 200

    requests.request("UNLOCK", f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer, data=lock)


def test_update_state_rejected_with_wrong_lock_id(bearer, workspace):
    lock = lock_body()

    requests.request("LOCK", f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer, data=lock)

    res = requests.post(
        f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}?ID=wrong-id",
        headers=bearer,
        data=STATE,
    )
    assert res.status_code == 409

    requests.request("UNLOCK", f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer, data=lock)


def test_update_state_accepted_with_correct_lock_id(bearer, workspace):
    lock = lock_body()
    lock_data = json.loads(lock)

    requests.request("LOCK", f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer, data=lock)

    res = requests.post(
        f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}?ID={lock_data['ID']}",
        headers=bearer,
        data=STATE,
    )
    assert res.status_code == 200

    requests.request("UNLOCK", f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer, data=lock)


def test_basic_auth_accepted(basic_auth, workspace):
    res = requests.get(f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=basic_auth)
    assert res.status_code == 204


def test_unlock_without_lock_returns_200(bearer, workspace):
    res = requests.request("UNLOCK", f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer, data=lock_body())
    assert res.status_code == 200


def test_delete_nonexistent_state_returns_200(bearer, workspace):
    res = requests.delete(f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=bearer)
    assert res.status_code == 200


def test_cross_user_access_returns_403(other_bearer, workspace):
    res = requests.get(f"{BLUEPRINTS_URL}/state/{USERNAME}/{workspace}", headers=other_bearer)
    assert res.status_code == 403


