"""Integration tests for the secrets API.

Secrets require:
  1. GATEKEEPER_SECRETS_KEY to be configured (endpoints return 503 if not set).
  2. The calling user to belong to an org (secrets are org-scoped).
  3. Appropriate RBAC permissions (createSecret, listSecret, updateSecret,
     deleteSecret, getSecretProvider, updateSecretProvider, deleteSecretProvider).

The secrets_admin fixture creates a dedicated user with wildcard permissions
who creates an org, which auto-assigns them to it.
"""

import json as _json
import os
import uuid
import pytest
import requests
import sqlalchemy as sa


def _connect():
    url = os.getenv(
        "DATABASE_URL", "postgresql://postgres:postgres@127.0.0.1:5432/gatekeeper"
    ).replace("postgresql", "postgresql+psycopg2")
    return sa.create_engine(url).connect()


def bearer(token):
    return {"Authorization": f"Bearer {token}"}


def rand_id():
    return uuid.uuid4().hex[:8]


def _skip_if_unavailable(resp):
    if resp.status_code == 503:
        pytest.skip("secrets not available: GATEKEEPER_SECRETS_KEY not configured")


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session")
def secrets_admin(base_url):
    """Admin user with wildcard permissions who belongs to an org.

    POST /orgs auto-assigns the creator's org_id, satisfying the org-membership
    requirement for all secrets endpoints.
    """
    uid = rand_id()
    email = f"secrets_admin_{uid}@example.com"
    password = "s3cr3tsP4ss!"

    resp = requests.post(
        f"{base_url}/signup",
        json={"email": email, "username": f"secrets_admin_{uid}", "password": password},
    )
    assert resp.status_code == 201, f"secrets_admin signup failed: {resp.text}"
    user_id = resp.json()["user_id"]

    conn = _connect()
    perm_id = str(uuid.uuid4())
    role_id = str(uuid.uuid4())
    conn.execute(
        sa.text("""
            INSERT INTO permissions
                (permissions_id, name, service, actions, resources, active, created_at, updated_at)
            VALUES (:pid, 'secrets-admin-perms', 'gatekeeper',
                    '["*"]'::jsonb, '["*"]'::jsonb, true, NOW(), NOW())
        """),
        {"pid": perm_id},
    )
    conn.execute(
        sa.text("""
            INSERT INTO roles (role_id, permissions_ids, active, created_at, updated_at)
            VALUES (:rid, CAST(:pids AS jsonb), true, NOW(), NOW())
        """),
        {"rid": role_id, "pids": _json.dumps([perm_id])},
    )
    conn.execute(
        sa.text("UPDATE users SET role_id = :rid WHERE user_id = :uid"),
        {"rid": role_id, "uid": user_id},
    )
    conn.commit()
    conn.close()

    login = requests.post(f"{base_url}/login", json={"email": email, "password": password})
    assert login.status_code == 200, f"secrets_admin login failed: {login.text}"
    token = login.json()["token"]

    org_resp = requests.post(
        f"{base_url}/orgs",
        json={"org_name": f"secrets-org-{uid}"},
        headers=bearer(token),
    )
    assert org_resp.status_code == 201, f"secrets_admin org creation failed: {org_resp.text}"
    org_id = org_resp.json()["org_id"]

    return {"token": token, "user_id": user_id, "org_id": org_id}


@pytest.fixture
def secret(base_url, secrets_admin):
    """Create a secret and soft-delete it after the test."""
    resp = requests.post(
        f"{base_url}/secrets",
        json={"name": f"TEST_{rand_id().upper()}", "value": "fixture-value"},
        headers=bearer(secrets_admin["token"]),
    )
    _skip_if_unavailable(resp)
    assert resp.status_code == 201, f"secret fixture failed: {resp.text}"
    data = resp.json()

    yield data

    requests.delete(
        f"{base_url}/secrets/{data['secret_id']}",
        headers=bearer(secrets_admin["token"]),
    )


# ---------------------------------------------------------------------------
# TestCreateSecret
# ---------------------------------------------------------------------------


class TestCreateSecret:
    def test_no_auth_returns_401(self, base_url):
        resp = requests.post(f"{base_url}/secrets", json={"name": "X", "value": "y"})
        assert resp.status_code == 401

    def test_unprivileged_user_returns_403(self, base_url, token):
        resp = requests.post(
            f"{base_url}/secrets",
            json={"name": "X", "value": "y"},
            headers=bearer(token),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 403

    def test_missing_value_returns_400(self, base_url, secrets_admin):
        resp = requests.post(
            f"{base_url}/secrets",
            json={"name": "ONLY_NAME"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 400

    def test_missing_name_returns_400(self, base_url, secrets_admin):
        resp = requests.post(
            f"{base_url}/secrets",
            json={"value": "only-value"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 400

    def test_invalid_name_character_returns_400(self, base_url, secrets_admin):
        resp = requests.post(
            f"{base_url}/secrets",
            json={"name": "INVALID NAME!", "value": "val"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 400

    def test_success_returns_201(self, base_url, secrets_admin):
        name = f"CREATE_{rand_id().upper()}"
        resp = requests.post(
            f"{base_url}/secrets",
            json={"name": name, "value": "my-value"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 201
        requests.delete(
            f"{base_url}/secrets/{resp.json()['secret_id']}",
            headers=bearer(secrets_admin["token"]),
        )

    def test_response_contains_expected_fields(self, base_url, secrets_admin):
        name = f"FIELDS_{rand_id().upper()}"
        resp = requests.post(
            f"{base_url}/secrets",
            json={"name": name, "value": "check-fields"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 201
        body = resp.json()
        assert body.get("secret_id")
        assert body["name"] == name
        assert body["org_id"] == secrets_admin["org_id"]
        requests.delete(
            f"{base_url}/secrets/{body['secret_id']}",
            headers=bearer(secrets_admin["token"]),
        )

    def test_response_omits_plaintext_value(self, base_url, secrets_admin):
        name = f"NOVAL_{rand_id().upper()}"
        resp = requests.post(
            f"{base_url}/secrets",
            json={"name": name, "value": "hidden"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 201
        body = resp.json()
        assert "value" not in body
        assert "ciphertext" not in body
        requests.delete(
            f"{base_url}/secrets/{body['secret_id']}",
            headers=bearer(secrets_admin["token"]),
        )

    def test_duplicate_name_returns_409(self, base_url, secrets_admin, secret):
        resp = requests.post(
            f"{base_url}/secrets",
            json={"name": secret["name"], "value": "duplicate"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 409


# ---------------------------------------------------------------------------
# TestListSecrets
# ---------------------------------------------------------------------------


class TestListSecrets:
    def test_no_auth_returns_401(self, base_url):
        resp = requests.get(f"{base_url}/secrets")
        assert resp.status_code == 401

    def test_unprivileged_user_returns_403(self, base_url, token):
        resp = requests.get(f"{base_url}/secrets", headers=bearer(token))
        _skip_if_unavailable(resp)
        assert resp.status_code == 403

    def test_returns_200(self, base_url, secrets_admin):
        resp = requests.get(f"{base_url}/secrets", headers=bearer(secrets_admin["token"]))
        _skip_if_unavailable(resp)
        assert resp.status_code == 200

    def test_returns_array(self, base_url, secrets_admin):
        resp = requests.get(f"{base_url}/secrets", headers=bearer(secrets_admin["token"]))
        _skip_if_unavailable(resp)
        assert isinstance(resp.json(), list)

    def test_created_secret_appears(self, base_url, secrets_admin, secret):
        resp = requests.get(f"{base_url}/secrets", headers=bearer(secrets_admin["token"]))
        _skip_if_unavailable(resp)
        ids = [s["secret_id"] for s in resp.json()]
        assert secret["secret_id"] in ids

    def test_response_omits_plaintext_value(self, base_url, secrets_admin, secret):
        resp = requests.get(f"{base_url}/secrets", headers=bearer(secrets_admin["token"]))
        _skip_if_unavailable(resp)
        for item in resp.json():
            assert "value" not in item
            assert "ciphertext" not in item

    def test_deleted_secret_not_in_list(self, base_url, secrets_admin):
        name = f"DEL_LIST_{rand_id().upper()}"
        create = requests.post(
            f"{base_url}/secrets",
            json={"name": name, "value": "temp"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(create)
        secret_id = create.json()["secret_id"]

        requests.delete(
            f"{base_url}/secrets/{secret_id}",
            headers=bearer(secrets_admin["token"]),
        )

        resp = requests.get(f"{base_url}/secrets", headers=bearer(secrets_admin["token"]))
        ids = [s["secret_id"] for s in resp.json()]
        assert secret_id not in ids


# ---------------------------------------------------------------------------
# TestUpdateSecret
# ---------------------------------------------------------------------------


class TestUpdateSecret:
    def test_no_auth_returns_401(self, base_url):
        resp = requests.put(f"{base_url}/secrets/{rand_id()}", json={"value": "x"})
        assert resp.status_code == 401

    def test_unprivileged_user_returns_403(self, base_url, token, secret):
        resp = requests.put(
            f"{base_url}/secrets/{secret['secret_id']}",
            json={"value": "x"},
            headers=bearer(token),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 403

    def test_nonexistent_returns_404(self, base_url, secrets_admin):
        resp = requests.put(
            f"{base_url}/secrets/{rand_id()}",
            json={"value": "x"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 404

    def test_missing_value_returns_400(self, base_url, secrets_admin, secret):
        resp = requests.put(
            f"{base_url}/secrets/{secret['secret_id']}",
            json={},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 400

    def test_success_returns_200(self, base_url, secrets_admin, secret):
        resp = requests.put(
            f"{base_url}/secrets/{secret['secret_id']}",
            json={"value": "updated-value"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 200

    def test_can_rename_secret(self, base_url, secrets_admin, secret):
        new_name = f"RENAMED_{rand_id().upper()}"
        resp = requests.put(
            f"{base_url}/secrets/{secret['secret_id']}",
            json={"name": new_name, "value": "updated"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 200
        assert resp.json()["name"] == new_name

    def test_response_omits_plaintext_value(self, base_url, secrets_admin, secret):
        resp = requests.put(
            f"{base_url}/secrets/{secret['secret_id']}",
            json={"value": "some-value"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        body = resp.json()
        assert "value" not in body
        assert "ciphertext" not in body


# ---------------------------------------------------------------------------
# TestDeleteSecret
# ---------------------------------------------------------------------------


class TestDeleteSecret:
    def test_no_auth_returns_401(self, base_url):
        resp = requests.delete(f"{base_url}/secrets/{rand_id()}")
        assert resp.status_code == 401

    def test_unprivileged_user_returns_403(self, base_url, token, secret):
        resp = requests.delete(
            f"{base_url}/secrets/{secret['secret_id']}",
            headers=bearer(token),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 403

    def test_nonexistent_returns_404(self, base_url, secrets_admin):
        resp = requests.delete(
            f"{base_url}/secrets/{rand_id()}",
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 404

    def test_success_returns_204(self, base_url, secrets_admin):
        name = f"DEL_OK_{rand_id().upper()}"
        create = requests.post(
            f"{base_url}/secrets",
            json={"name": name, "value": "to-delete"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(create)
        secret_id = create.json()["secret_id"]

        resp = requests.delete(
            f"{base_url}/secrets/{secret_id}",
            headers=bearer(secrets_admin["token"]),
        )
        assert resp.status_code == 204

    def test_deleted_secret_absent_from_list(self, base_url, secrets_admin):
        name = f"DEL_GONE_{rand_id().upper()}"
        create = requests.post(
            f"{base_url}/secrets",
            json={"name": name, "value": "transient"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(create)
        secret_id = create.json()["secret_id"]

        requests.delete(
            f"{base_url}/secrets/{secret_id}",
            headers=bearer(secrets_admin["token"]),
        )

        resp = requests.get(f"{base_url}/secrets", headers=bearer(secrets_admin["token"]))
        ids = [s["secret_id"] for s in resp.json()]
        assert secret_id not in ids


# ---------------------------------------------------------------------------
# TestSecretProvider
# ---------------------------------------------------------------------------


class TestSecretProvider:
    def test_get_no_auth_returns_401(self, base_url, secrets_admin):
        resp = requests.get(f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider")
        assert resp.status_code == 401

    def test_get_unprivileged_returns_403(self, base_url, token, secrets_admin):
        resp = requests.get(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            headers=bearer(token),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 403

    def test_get_not_configured_returns_404(self, base_url, secrets_admin):
        # Use a fresh org that has never had a provider set.
        uid = rand_id()
        org_resp = requests.post(
            f"{base_url}/orgs",
            json={"org_name": f"provider-test-{uid}"},
            headers=bearer(secrets_admin["token"]),
        )
        assert org_resp.status_code == 201
        fresh_org_id = org_resp.json()["org_id"]

        resp = requests.get(
            f"{base_url}/orgs/{fresh_org_id}/secret-provider",
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 404

        requests.delete(f"{base_url}/orgs/{fresh_org_id}", headers=bearer(secrets_admin["token"]))

    def test_set_no_auth_returns_401(self, base_url, secrets_admin):
        resp = requests.put(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            json={"provider": "builtin"},
        )
        assert resp.status_code == 401

    def test_set_unprivileged_returns_403(self, base_url, token, secrets_admin):
        resp = requests.put(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            json={"provider": "builtin"},
            headers=bearer(token),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 403

    def test_set_invalid_provider_returns_400(self, base_url, secrets_admin):
        resp = requests.put(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            json={"provider": "unknown-provider"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 400

    def test_set_builtin_returns_200(self, base_url, secrets_admin):
        resp = requests.put(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            json={"provider": "builtin"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 200
        body = resp.json()
        assert body["provider"] == "builtin"
        assert body["org_id"] == secrets_admin["org_id"]

    def test_get_after_set_returns_provider(self, base_url, secrets_admin):
        set_resp = requests.put(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            json={"provider": "builtin"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(set_resp)
        assert set_resp.status_code == 200

        get_resp = requests.get(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            headers=bearer(secrets_admin["token"]),
        )
        assert get_resp.status_code == 200
        assert get_resp.json()["provider"] == "builtin"

    def test_delete_no_auth_returns_401(self, base_url, secrets_admin):
        resp = requests.delete(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider"
        )
        assert resp.status_code == 401

    def test_delete_unprivileged_returns_403(self, base_url, token, secrets_admin):
        resp = requests.delete(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            headers=bearer(token),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 403

    def test_delete_returns_204(self, base_url, secrets_admin):
        set_resp = requests.put(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            json={"provider": "builtin"},
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(set_resp)

        resp = requests.delete(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            headers=bearer(secrets_admin["token"]),
        )
        assert resp.status_code == 204

    def test_vault_with_private_address_returns_400(self, base_url, secrets_admin):
        resp = requests.put(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            json={
                "provider": "vault",
                "config": {"address": "http://192.168.1.1:8200", "token": "tok"},
            },
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 400

    def test_vault_with_loopback_address_returns_400(self, base_url, secrets_admin):
        resp = requests.put(
            f"{base_url}/orgs/{secrets_admin['org_id']}/secret-provider",
            json={
                "provider": "vault",
                "config": {"address": "http://127.0.0.1:8200", "token": "tok"},
            },
            headers=bearer(secrets_admin["token"]),
        )
        _skip_if_unavailable(resp)
        assert resp.status_code == 400
