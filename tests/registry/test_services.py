"""
Integration tests for the registry service.

Requires a live registry stack. Configure via environment variables:
  REGISTRY_URL           (default: http://localhost:8084)
  REGISTRY_ADMIN_KEY     (default: registry-admin-local-secret)
  REGISTRY_READ_KEY      (default: registry-read-local-secret)
"""

import uuid

import requests


# ---------------------------------------------------------------------------
# TestAuth — auth rejection tests (no DB state needed)
# ---------------------------------------------------------------------------


class TestAuth:
    def test_get_no_key_returns_401(self, registry_url):
        resp = requests.get(f"{registry_url}/services")
        assert resp.status_code == 401

    def test_get_wrong_key_returns_401(self, registry_url):
        headers = {"Authorization": "Bearer definitely-wrong-key"}
        resp = requests.get(f"{registry_url}/services", headers=headers)
        assert resp.status_code == 401

    def test_post_no_key_returns_401(self, registry_url):
        resp = requests.post(f"{registry_url}/services", json={"name": "x", "url": "http://x"})
        assert resp.status_code == 401

    def test_post_wrong_key_returns_401(self, registry_url):
        headers = {"Authorization": "Bearer definitely-wrong-key"}
        resp = requests.post(
            f"{registry_url}/services",
            json={"name": "x", "url": "http://x"},
            headers=headers,
        )
        assert resp.status_code == 401

    def test_post_read_key_returns_401(self, registry_url, read_headers):
        # Read key is not sufficient for write operations.
        resp = requests.post(
            f"{registry_url}/services",
            json={"name": "x", "url": "http://x"},
            headers=read_headers,
        )
        assert resp.status_code == 401

    def test_delete_no_key_returns_401(self, registry_url):
        resp = requests.delete(f"{registry_url}/services/some-nonexistent-id")
        assert resp.status_code == 401

    def test_delete_wrong_key_returns_401(self, registry_url):
        headers = {"Authorization": "Bearer definitely-wrong-key"}
        resp = requests.delete(
            f"{registry_url}/services/some-nonexistent-id",
            headers=headers,
        )
        assert resp.status_code == 401


# ---------------------------------------------------------------------------
# TestListServices — GET /services
# ---------------------------------------------------------------------------


class TestListServices:
    def test_read_key_returns_200(self, registry_url, read_headers):
        resp = requests.get(f"{registry_url}/services", headers=read_headers)
        assert resp.status_code == 200

    def test_admin_key_returns_200(self, registry_url, admin_headers):
        resp = requests.get(f"{registry_url}/services", headers=admin_headers)
        assert resp.status_code == 200

    def test_returns_array(self, registry_url, read_headers):
        resp = requests.get(f"{registry_url}/services", headers=read_headers)
        assert resp.status_code == 200
        body = resp.json()
        assert isinstance(body, list)

    def test_created_service_appears_in_list(self, registry_url, admin_headers, read_headers, service):
        resp = requests.get(f"{registry_url}/services", headers=read_headers)
        assert resp.status_code == 200
        service_ids = [s["service_id"] for s in resp.json()]
        assert service["service_id"] in service_ids


# ---------------------------------------------------------------------------
# TestCreateService — POST /services
# ---------------------------------------------------------------------------


class TestCreateService:
    def test_returns_201(self, registry_url, admin_headers):
        unique = uuid.uuid4().hex[:8]
        payload = {"name": f"create-test-{unique}", "url": f"http://create-{unique}.local"}
        resp = requests.post(f"{registry_url}/services", json=payload, headers=admin_headers)
        service_id = resp.json().get("service_id", "")
        # Teardown
        if service_id:
            requests.delete(f"{registry_url}/services/{service_id}", headers=admin_headers)
        assert resp.status_code == 201

    def test_response_contains_service_id(self, registry_url, admin_headers):
        unique = uuid.uuid4().hex[:8]
        payload = {"name": f"id-test-{unique}", "url": f"http://id-{unique}.local"}
        resp = requests.post(f"{registry_url}/services", json=payload, headers=admin_headers)
        body = resp.json()
        service_id = body.get("service_id", "")
        if service_id:
            requests.delete(f"{registry_url}/services/{service_id}", headers=admin_headers)
        assert resp.status_code == 201
        assert "service_id" in body
        assert body["service_id"] != ""

    def test_response_reflects_name_and_url(self, registry_url, admin_headers):
        unique = uuid.uuid4().hex[:8]
        name = f"reflect-test-{unique}"
        url = f"http://reflect-{unique}.local"
        payload = {"name": name, "url": url}
        resp = requests.post(f"{registry_url}/services", json=payload, headers=admin_headers)
        body = resp.json()
        service_id = body.get("service_id", "")
        if service_id:
            requests.delete(f"{registry_url}/services/{service_id}", headers=admin_headers)
        assert resp.status_code == 201
        assert body["name"] == name
        assert body["url"] == url

    def test_missing_name_returns_400(self, registry_url, admin_headers):
        resp = requests.post(
            f"{registry_url}/services",
            json={"url": "http://no-name.local"},
            headers=admin_headers,
        )
        assert resp.status_code == 400

    def test_missing_url_returns_400(self, registry_url, admin_headers):
        resp = requests.post(
            f"{registry_url}/services",
            json={"name": "no-url-svc"},
            headers=admin_headers,
        )
        assert resp.status_code == 400

    def test_duplicate_name_returns_409(self, registry_url, admin_headers, service):
        # service fixture already created a service; post with the same name.
        resp = requests.post(
            f"{registry_url}/services",
            json={"name": service["name"], "url": "http://duplicate.local"},
            headers=admin_headers,
        )
        assert resp.status_code == 409


# ---------------------------------------------------------------------------
# TestDeleteService — DELETE /services/{id}
# ---------------------------------------------------------------------------


class TestDeleteService:
    def test_returns_204(self, registry_url, admin_headers):
        unique = uuid.uuid4().hex[:8]
        payload = {"name": f"delete-me-{unique}", "url": f"http://delete-me-{unique}.local"}
        create_resp = requests.post(f"{registry_url}/services", json=payload, headers=admin_headers)
        assert create_resp.status_code == 201
        service_id = create_resp.json()["service_id"]

        delete_resp = requests.delete(f"{registry_url}/services/{service_id}", headers=admin_headers)
        assert delete_resp.status_code == 204

    def test_deleted_service_not_in_list(self, registry_url, admin_headers, read_headers):
        unique = uuid.uuid4().hex[:8]
        payload = {"name": f"gone-{unique}", "url": f"http://gone-{unique}.local"}
        create_resp = requests.post(f"{registry_url}/services", json=payload, headers=admin_headers)
        assert create_resp.status_code == 201
        service_id = create_resp.json()["service_id"]

        requests.delete(f"{registry_url}/services/{service_id}", headers=admin_headers)

        list_resp = requests.get(f"{registry_url}/services", headers=read_headers)
        assert list_resp.status_code == 200
        service_ids = [s["service_id"] for s in list_resp.json()]
        assert service_id not in service_ids

    def test_not_found_returns_404(self, registry_url, admin_headers):
        nonexistent_id = uuid.uuid4().hex
        resp = requests.delete(f"{registry_url}/services/{nonexistent_id}", headers=admin_headers)
        assert resp.status_code == 404

    def test_double_delete_returns_404(self, registry_url, admin_headers):
        unique = uuid.uuid4().hex[:8]
        payload = {"name": f"double-del-{unique}", "url": f"http://double-del-{unique}.local"}
        create_resp = requests.post(f"{registry_url}/services", json=payload, headers=admin_headers)
        assert create_resp.status_code == 201
        service_id = create_resp.json()["service_id"]

        first = requests.delete(f"{registry_url}/services/{service_id}", headers=admin_headers)
        assert first.status_code == 204

        second = requests.delete(f"{registry_url}/services/{service_id}", headers=admin_headers)
        assert second.status_code == 404


