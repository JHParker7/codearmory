"""Shared fixtures for argo integration tests.

The argo service holds no Argo CD credentials — it drives an outpost via the
outpost-gateway and records the app state the outpost reports. These tests act
as the outpost (enroll, poll sync commands, post app-state events) so the sync
flow runs end-to-end without a real Argo CD.
"""
import os
import time
import uuid

import pytest
import requests
import sqlalchemy as sa

ARGO_URL = os.getenv("ARGO_URL", "http://localhost:8091")
GATEWAY_URL = os.getenv("OUTPOST_GATEWAY_URL", "http://localhost:8092")
GATEKEEPER_URL = os.getenv("GATEKEEPER_URL", "http://localhost:8080")

# Per-suite email prefixes used by _signup_login; cleanup deletes users matching these.
USER_EMAIL_LIKE = ["argo_test%@example.com", "argo_other%@example.com"]
# In the compose test network the DB host is "postgres"; override via env for local runs.
GATEKEEPER_DB_URL = os.getenv(
    "GATEKEEPER_DB_URL", "postgresql://postgres:postgres@postgres:5432/gatekeeper"
)
ARGO_DB_URL = os.getenv(
    "ARGO_DB_URL", "postgresql://postgres:postgres@postgres:5432/argo"
)


def _engine(url):
    return sa.create_engine(url.replace("postgresql://", "postgresql+psycopg2://"))


def _test_user_rows(conn, email_like):
    clause = " OR ".join(f"email LIKE :p{i}" for i in range(len(email_like)))
    params = {f"p{i}": pat for i, pat in enumerate(email_like)}
    return conn.execute(
        sa.text(f"SELECT user_id, role_id FROM users WHERE {clause}"), params
    ).fetchall()


def _suite_user_ids():
    conn = None
    try:
        conn = _engine(GATEKEEPER_DB_URL).connect()
        return [str(r[0]) for r in _test_user_rows(conn, USER_EMAIL_LIKE)]
    except Exception as e:
        print(f"Gatekeeper user lookup error: {e}")
        return []
    finally:
        if conn is not None:
            conn.close()


def _cleanup_gatekeeper_users(email_like):
    """Delete test users (and their default role/permissions/sessions) so repeated
    runs don't accumulate rows in the shared gatekeeper DB."""
    conn = None
    try:
        conn = _engine(GATEKEEPER_DB_URL).connect()
        rows = _test_user_rows(conn, email_like)
        if not rows:
            return
        user_ids = [str(r[0]) for r in rows]
        role_ids = [str(r[1]) for r in rows if r[1] is not None]
        perm_ids = []
        if role_ids:
            for row in conn.execute(
                sa.text("SELECT permissions_ids FROM roles WHERE role_id = ANY(:rids) AND permissions_ids IS NOT NULL"),
                {"rids": role_ids},
            ):
                if row[0]:
                    perm_ids.extend([str(p) for p in row[0]])
        conn.execute(sa.text("DELETE FROM sessions WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("DELETE FROM permissions_checks WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("UPDATE users SET role_id = NULL WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("DELETE FROM users WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        if role_ids:
            conn.execute(sa.text("DELETE FROM roles WHERE role_id = ANY(:rids)"), {"rids": role_ids})
        if perm_ids:
            conn.execute(sa.text("DELETE FROM permissions WHERE permissions_id = ANY(:pids)"), {"pids": perm_ids})
        conn.commit()
    except Exception as e:
        print(f"Gatekeeper user cleanup error: {e}")
    finally:
        if conn is not None:
            conn.close()


def pytest_sessionfinish(session, exitstatus):
    # Delete syncs + apps owned by this suite's test users, then the users themselves.
    user_ids = _suite_user_ids()
    conn = None
    try:
        conn = _engine(ARGO_DB_URL).connect()
        if user_ids:
            conn.execute(sa.text("DELETE FROM syncs WHERE user_id = ANY(:uids)"), {"uids": user_ids})
            conn.execute(sa.text("DELETE FROM apps WHERE user_id = ANY(:uids)"), {"uids": user_ids})
            conn.commit()
    except Exception as e:
        print(f"Argo cleanup error: {e}")
    finally:
        if conn is not None:
            conn.close()
    _cleanup_gatekeeper_users(USER_EMAIL_LIKE)


def _signup_login(prefix):
    email = f"{prefix}_{uuid.uuid4().hex[:8]}@example.com"
    password = f"{prefix}_pass"
    username = f"{prefix}_{uuid.uuid4().hex[:8]}"
    requests.post(f"{GATEKEEPER_URL}/signup", json={
        "email": email, "username": username, "password": password,
    })
    res = requests.post(f"{GATEKEEPER_URL}/login", json={"email": email, "password": password})
    assert res.status_code == 200, f"login failed: {res.text}"
    return res.json()["token"]


@pytest.fixture(scope="session")
def token():
    return _signup_login("argo_test")


@pytest.fixture(scope="session")
def other_token():
    return _signup_login("argo_other")


@pytest.fixture(scope="session")
def bearer(token):
    return {"Authorization": f"Bearer {token}"}


@pytest.fixture(scope="session")
def other_bearer(other_token):
    return {"Authorization": f"Bearer {other_token}"}


class OutpostSim:
    def __init__(self, outpost_id, key):
        self.outpost_id = outpost_id
        self.key = key

    @property
    def headers(self):
        return {"X-Outpost-ID": self.outpost_id, "Authorization": f"Bearer {self.key}"}

    def poll_commands(self, timeout=35):
        return requests.get(f"{GATEWAY_URL}/outpost/commands", headers=self.headers, timeout=timeout)

    def ack(self, command_id):
        return requests.post(f"{GATEWAY_URL}/outpost/commands/{command_id}/ack", headers=self.headers)

    def post_event(self, integration, type_, payload):
        return requests.post(f"{GATEWAY_URL}/outpost/events", headers=self.headers,
                            json={"integration": integration, "type": type_, "payload": payload})

    def wait_for_command(self, type_, attempts=3):
        for _ in range(attempts):
            res = self.poll_commands()
            if res.status_code != 200:
                continue
            for c in res.json():
                self.ack(c["id"])
                if c["type"] == type_:
                    return c
        return None


@pytest.fixture
def outpost(bearer):
    res = requests.post(f"{GATEWAY_URL}/outposts", headers=bearer,
                        json={"name": f"argo-{uuid.uuid4().hex[:6]}", "modules": ["argo"]})
    assert res.status_code == 201, res.text
    outpost_id = res.json()["outpost_id"]
    reg = requests.post(f"{GATEWAY_URL}/outpost/register",
                       json={"enrollment_token": res.json()["enrollment_token"]})
    assert reg.status_code == 201, reg.text
    return OutpostSim(outpost_id, reg.json()["outpost_key"])


def poll_sync(bearer, sync_id, until, timeout=40):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        res = requests.get(f"{ARGO_URL}/syncs/{sync_id}", headers=bearer)
        if res.status_code == 200:
            last = res.json()
            if last["status"] in until:
                return last
        time.sleep(1)
    return last
