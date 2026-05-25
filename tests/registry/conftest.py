import os
import uuid

import pytest
import requests


@pytest.fixture(scope="session")
def registry_url():
    return os.getenv("REGISTRY_URL", "http://localhost:8084")


@pytest.fixture(scope="session")
def admin_key():
    return os.getenv("REGISTRY_ADMIN_KEY", "registry-admin-local-secret")


@pytest.fixture(scope="session")
def read_key():
    return os.getenv("REGISTRY_READ_KEY", "registry-read-local-secret")


@pytest.fixture
def admin_headers(admin_key):
    return {"Authorization": f"Bearer {admin_key}"}


@pytest.fixture
def read_headers(read_key):
    return {"Authorization": f"Bearer {read_key}"}


@pytest.fixture
def service(registry_url, admin_headers):
    """
    Create a unique service via POST /services and yield its response body.
    Best-effort DELETE after the test via DELETE /services/{service_id}.
    """
    unique_suffix = uuid.uuid4().hex[:8]
    payload = {
        "name": f"test-svc-{unique_suffix}",
        "url": f"http://test-svc-{unique_suffix}.local",
    }
    resp = requests.post(
        f"{registry_url}/services",
        json=payload,
        headers=admin_headers,
    )
    resp.raise_for_status()
    body = resp.json()

    yield body

    # Teardown — best-effort, ignore failures
    service_id = body.get("service_id", "")
    if service_id:
        try:
            requests.delete(
                f"{registry_url}/services/{service_id}",
                headers=admin_headers,
            )
        except Exception:
            pass
