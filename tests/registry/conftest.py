import os
import uuid

import pytest
import requests


@pytest.fixture(scope="session")
def registry_url():
    return os.getenv("REGISTRY_URL", "http://localhost:8084")


@pytest.fixture(scope="session")
def admin_key():
    return os.getenv("REGISTRY_ADMIN_KEY", "registry-admin:registry-admin-local-secret")


@pytest.fixture(scope="session")
def read_key():
    return os.getenv("REGISTRY_READ_KEY", "conductor:registry-read-local-secret")


@pytest.fixture(scope="session")
def test_reader_account():
    """A throwaway read-role account seeded only for tests, so rotation tests can
    mutate its key without disrupting the conductor account the live stack uses."""
    name = os.getenv("REGISTRY_TEST_READER_NAME", "registry-test-reader")
    bootstrap = os.getenv(
        "REGISTRY_TEST_READER_KEY", "registry-test-reader-local-secret"
    )
    return name, bootstrap


@pytest.fixture
def admin_headers(admin_key):
    return {"X-Service-Key": admin_key}


@pytest.fixture
def read_headers(read_key):
    return {"X-Service-Key": read_key}


@pytest.fixture
def service(registry_url, admin_headers):
    """
    Create a unique service via POST /services and yield its response body.
    Best-effort DELETE after the test via DELETE /services/{service_id}.
    """
    unique_suffix = uuid.uuid4().hex[:8]
    payload = {
        "name": f"test-svc-{unique_suffix}",
        "url": "http://203.0.113.1:9000",
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
