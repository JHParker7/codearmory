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
        headers = {"X-Service-Key": "conductor:definitely-wrong-key"}
        resp = requests.get(f"{registry_url}/services", headers=headers)
        assert resp.status_code == 401

    def test_post_no_key_returns_401(self, registry_url):
        resp = requests.post(f"{registry_url}/services", json={"name": "x", "url": "http://x"})
        assert resp.status_code == 401

    def test_post_wrong_key_returns_401(self, registry_url):
        headers = {"X-Service-Key": "registry-admin:definitely-wrong-key"}
        resp = requests.post(
            f"{registry_url}/services",
            json={"name": "x", "url": "http://x"},
            headers=headers,
        )
        assert resp.status_code == 401

    def test_post_read_key_returns_403(self, registry_url, read_headers):
        # Read key is not sufficient for write operations.
        resp = requests.post(
            f"{registry_url}/services",
            json={"name": "x", "url": "http://x"},
            headers=read_headers,
        )
        assert resp.status_code == 403

    def test_delete_no_key_returns_401(self, registry_url):
        resp = requests.delete(f"{registry_url}/services/some-nonexistent-id")
        assert resp.status_code == 401

    def test_delete_wrong_key_returns_401(self, registry_url):
        headers = {"X-Service-Key": "registry-admin:definitely-wrong-key"}
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
        payload = {"name": f"create-test-{unique}", "url": "http://203.0.113.1:9000"}
        resp = requests.post(f"{registry_url}/services", json=payload, headers=admin_headers)
        service_id = resp.json().get("service_id", "")
        # Teardown
        if service_id:
            requests.delete(f"{registry_url}/services/{service_id}", headers=admin_headers)
        assert resp.status_code == 201

    def test_response_contains_service_id(self, registry_url, admin_headers):
        unique = uuid.uuid4().hex[:8]
        payload = {"name": f"id-test-{unique}", "url": "http://203.0.113.1:9000"}
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
        url = "http://203.0.113.1:9000"
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
            json={"url": "http://203.0.113.1:9000"},
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
            json={"name": service["name"], "url": "http://203.0.113.1:9000"},
            headers=admin_headers,
        )
        assert resp.status_code == 409


# ---------------------------------------------------------------------------
# TestDeleteService — DELETE /services/{id}
# ---------------------------------------------------------------------------


class TestDeleteService:
    def test_returns_204(self, registry_url, admin_headers):
        unique = uuid.uuid4().hex[:8]
        payload = {"name": f"delete-me-{unique}", "url": "http://203.0.113.1:9000"}
        create_resp = requests.post(f"{registry_url}/services", json=payload, headers=admin_headers)
        assert create_resp.status_code == 201
        service_id = create_resp.json()["service_id"]

        delete_resp = requests.delete(f"{registry_url}/services/{service_id}", headers=admin_headers)
        assert delete_resp.status_code == 204

    def test_deleted_service_not_in_list(self, registry_url, admin_headers, read_headers):
        unique = uuid.uuid4().hex[:8]
        payload = {"name": f"gone-{unique}", "url": "http://203.0.113.1:9000"}
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
        payload = {"name": f"double-del-{unique}", "url": "http://203.0.113.1:9000"}
        create_resp = requests.post(f"{registry_url}/services", json=payload, headers=admin_headers)
        assert create_resp.status_code == 201
        service_id = create_resp.json()["service_id"]

        first = requests.delete(f"{registry_url}/services/{service_id}", headers=admin_headers)
        assert first.status_code == 204

        second = requests.delete(f"{registry_url}/services/{service_id}", headers=admin_headers)
        assert second.status_code == 404


# ---------------------------------------------------------------------------
# TestSystemHealth — GET /system_health
# ---------------------------------------------------------------------------


class TestSystemHealth:
    def test_no_key_returns_401(self, registry_url):
        resp = requests.get(f"{registry_url}/system_health")
        assert resp.status_code == 401

    def test_wrong_key_returns_401(self, registry_url):
        resp = requests.get(
            f"{registry_url}/system_health",
            headers={"X-Service-Key": "conductor:definitely-wrong-key"},
        )
        assert resp.status_code == 401

    def test_read_key_returns_200(self, registry_url, read_headers):
        resp = requests.get(f"{registry_url}/system_health", headers=read_headers)
        assert resp.status_code == 200

    def test_response_has_status_field(self, registry_url, read_headers):
        resp = requests.get(f"{registry_url}/system_health", headers=read_headers)
        assert resp.status_code == 200
        body = resp.json()
        assert "status" in body
        assert body["status"] in ("healthy", "degraded")

    def test_response_has_services_map(self, registry_url, read_headers):
        resp = requests.get(f"{registry_url}/system_health", headers=read_headers)
        body = resp.json()
        assert "services" in body
        assert isinstance(body["services"], dict)


# ---------------------------------------------------------------------------
# TestListDefaultGrants — GET /default-grants
# ---------------------------------------------------------------------------


class TestListDefaultGrants:
    def test_no_key_returns_401(self, registry_url):
        resp = requests.get(f"{registry_url}/default-grants")
        assert resp.status_code == 401

    def test_wrong_key_returns_401(self, registry_url):
        resp = requests.get(
            f"{registry_url}/default-grants",
            headers={"X-Service-Key": "conductor:definitely-wrong-key"},
        )
        assert resp.status_code == 401

    def test_read_key_returns_200(self, registry_url, read_headers):
        resp = requests.get(f"{registry_url}/default-grants", headers=read_headers)
        assert resp.status_code == 200

    def test_returns_array(self, registry_url, read_headers):
        resp = requests.get(f"{registry_url}/default-grants", headers=read_headers)
        assert resp.status_code == 200
        assert isinstance(resp.json(), list)


# ---------------------------------------------------------------------------
# TestListActions — GET /actions
# ---------------------------------------------------------------------------


class TestListActions:
    def test_no_key_returns_401(self, registry_url):
        resp = requests.get(f"{registry_url}/actions")
        assert resp.status_code == 401

    def test_wrong_key_returns_401(self, registry_url):
        resp = requests.get(
            f"{registry_url}/actions",
            headers={"X-Service-Key": "conductor:definitely-wrong-key"},
        )
        assert resp.status_code == 401

    def test_read_key_returns_200(self, registry_url, read_headers):
        resp = requests.get(f"{registry_url}/actions", headers=read_headers)
        assert resp.status_code == 200

    def test_returns_array(self, registry_url, read_headers):
        resp = requests.get(f"{registry_url}/actions", headers=read_headers)
        assert resp.status_code == 200
        assert isinstance(resp.json(), list)


# ---------------------------------------------------------------------------
# TestUpdateServiceEndpoints — PUT /services/{id}/endpoints
# ---------------------------------------------------------------------------


class TestUpdateServiceEndpoints:
    def test_no_key_returns_401(self, registry_url):
        resp = requests.put(f"{registry_url}/services/{uuid.uuid4().hex}/endpoints", json={})
        assert resp.status_code == 401

    def test_read_key_returns_403(self, registry_url, read_headers):
        resp = requests.put(
            f"{registry_url}/services/{uuid.uuid4().hex}/endpoints",
            json={},
            headers=read_headers,
        )
        assert resp.status_code == 403

    def test_nonexistent_service_returns_404(self, registry_url, admin_headers):
        resp = requests.put(
            f"{registry_url}/services/{uuid.uuid4().hex}/endpoints",
            json={"endpoints": []},
            headers=admin_headers,
        )
        assert resp.status_code == 404

    def test_returns_204_on_success(self, registry_url, admin_headers, service):
        resp = requests.put(
            f"{registry_url}/services/{service['service_id']}/endpoints",
            json={
                "endpoints": [
                    {"method": "GET", "path": "/ping", "action": "read", "resource": "thing", "public": False},
                ],
            },
            headers=admin_headers,
        )
        assert resp.status_code == 204

    def test_endpoints_visible_in_service_list(self, registry_url, admin_headers, read_headers):
        unique = uuid.uuid4().hex[:8]
        create_resp = requests.post(
            f"{registry_url}/services",
            json={"name": f"ep-test-{unique}", "url": "http://203.0.113.1:9000"},
            headers=admin_headers,
        )
        assert create_resp.status_code == 201
        svc_id = create_resp.json()["service_id"]

        requests.put(
            f"{registry_url}/services/{svc_id}/endpoints",
            json={"endpoints": [{"method": "GET", "path": "/pong", "action": "read", "resource": "x", "public": True}]},
            headers=admin_headers,
        )

        list_resp = requests.get(f"{registry_url}/services", headers=read_headers)
        svc = next((s for s in list_resp.json() if s["service_id"] == svc_id), None)
        assert svc is not None
        assert len(svc["endpoints"]) == 1
        assert svc["endpoints"][0]["path"] == "/pong"

        requests.delete(f"{registry_url}/services/{svc_id}", headers=admin_headers)

    def test_replaces_existing_endpoints(self, registry_url, admin_headers, read_headers):
        unique = uuid.uuid4().hex[:8]
        create_resp = requests.post(
            f"{registry_url}/services",
            json={"name": f"ep-replace-{unique}", "url": "http://203.0.113.1:9000"},
            headers=admin_headers,
        )
        assert create_resp.status_code == 201
        svc_id = create_resp.json()["service_id"]

        requests.put(
            f"{registry_url}/services/{svc_id}/endpoints",
            json={"endpoints": [{"method": "GET", "path": "/old", "action": "read", "resource": "x", "public": False}]},
            headers=admin_headers,
        )
        requests.put(
            f"{registry_url}/services/{svc_id}/endpoints",
            json={"endpoints": [{"method": "POST", "path": "/new", "action": "write", "resource": "x", "public": False}]},
            headers=admin_headers,
        )

        list_resp = requests.get(f"{registry_url}/services", headers=read_headers)
        svc = next((s for s in list_resp.json() if s["service_id"] == svc_id), None)
        assert svc is not None
        paths = [ep["path"] for ep in svc["endpoints"]]
        assert "/new" in paths
        assert "/old" not in paths

        requests.delete(f"{registry_url}/services/{svc_id}", headers=admin_headers)


# ---------------------------------------------------------------------------
# TestRotateServiceKey — POST /service-accounts/rotate-key
# ---------------------------------------------------------------------------


class TestRotateServiceKey:
    def test_no_key_returns_401(self, registry_url):
        resp = requests.post(f"{registry_url}/service-accounts/rotate-key")
        assert resp.status_code == 401

    def test_wrong_key_returns_401(self, registry_url):
        resp = requests.post(
            f"{registry_url}/service-accounts/rotate-key",
            headers={"X-Service-Key": "conductor:definitely-wrong-key"},
        )
        assert resp.status_code == 401

    def test_malformed_key_header_returns_401(self, registry_url):
        resp = requests.post(
            f"{registry_url}/service-accounts/rotate-key",
            headers={"X-Service-Key": "no-colon-key"},
        )
        assert resp.status_code == 401

