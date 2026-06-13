"""Shared fixtures for outpost-gateway integration tests.

The gateway is the outpost-facing connection point: enrollment, command
long-poll, and event ingest, backed by a Postgres queue/outbox. These tests act
as both a portal user (creating outposts) and an outpost (enrolling, polling,
posting events) so the whole command/event plane is exercised without a cluster.
"""
import hashlib
import hmac
import os
import time
import uuid

import pytest
import requests
import sqlalchemy as sa

GATEWAY_URL = os.getenv("OUTPOST_GATEWAY_URL", "http://localhost:8092")
GATEKEEPER_URL = os.getenv("GATEKEEPER_URL", "http://localhost:8080")
INTERNAL_KEY = os.getenv("OUTPOST_INTERNAL_KEY", "outpost-internal-local-secret")

# Per-suite email prefixes used by _signup_login; cleanup deletes users matching these.
USER_EMAIL_LIKE = ["gw_test%@example.com", "gw_other%@example.com"]
# In the compose test network the DB host is "postgres"; override via env for local runs.
GATEKEEPER_DB_URL = os.getenv(
    "GATEKEEPER_DB_URL", "postgresql://postgres:postgres@postgres:5432/gatekeeper"
)
GATEWAY_DB_URL = os.getenv(
    "OUTPOST_GATEWAY_DB_URL", "postgresql://postgres:postgres@postgres:5432/outpost_gateway"
)


def _engine(url):
    return sa.create_engine(url.replace("postgresql://", "postgresql+psycopg2://"))


def _test_user_ids(conn, email_like):
    """Return the user_ids of test users matching this suite's email prefixes."""
    clause = " OR ".join(f"email LIKE :p{i}" for i in range(len(email_like)))
    params = {f"p{i}": pat for i, pat in enumerate(email_like)}
    rows = conn.execute(
        sa.text(f"SELECT user_id, role_id FROM users WHERE {clause}"), params
    ).fetchall()
    return rows


def _cleanup_gatekeeper_users(email_like):
    """Delete test users (and their default role/permissions/sessions) so repeated
    runs don't accumulate rows in the shared gatekeeper DB. Mirrors the gatekeeper
    suite's teardown but scoped to this suite's email prefixes."""
    conn = None
    try:
        conn = _engine(GATEKEEPER_DB_URL).connect()
        rows = _test_user_ids(conn, email_like)
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


def _suite_user_ids():
    """Fetch this suite's user_ids from gatekeeper so we can scope service-DB deletes."""
    conn = None
    try:
        conn = _engine(GATEKEEPER_DB_URL).connect()
        return [str(r[0]) for r in _test_user_ids(conn, USER_EMAIL_LIKE)]
    except Exception as e:
        print(f"Gatekeeper user lookup error: {e}")
        return []
    finally:
        if conn is not None:
            conn.close()


def pytest_sessionfinish(session, exitstatus):
    # Remove gateway rows (outposts + their queued commands/outbox events) owned by
    # this suite's test users, then remove the users themselves from gatekeeper.
    user_ids = _suite_user_ids()
    conn = None
    try:
        conn = _engine(GATEWAY_DB_URL).connect()
        if user_ids:
            op_ids = [str(r[0]) for r in conn.execute(
                sa.text("SELECT outpost_id FROM outposts WHERE user_id = ANY(:uids)"),
                {"uids": user_ids},
            ).fetchall()]
            if op_ids:
                conn.execute(sa.text("DELETE FROM outpost_commands WHERE outpost_id = ANY(:ids)"), {"ids": op_ids})
                conn.execute(sa.text("DELETE FROM outpost_events WHERE outpost_id = ANY(:ids)"), {"ids": op_ids})
                conn.execute(sa.text("DELETE FROM outposts WHERE outpost_id = ANY(:ids)"), {"ids": op_ids})
            conn.commit()
    except Exception as e:
        print(f"Gateway cleanup error: {e}")
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
    return _signup_login("gw_test")


@pytest.fixture(scope="session")
def other_token():
    return _signup_login("gw_other")


@pytest.fixture(scope="session")
def bearer(token):
    return {"Authorization": f"Bearer {token}"}


@pytest.fixture(scope="session")
def other_bearer(other_token):
    return {"Authorization": f"Bearer {other_token}"}


def sign_internal(domain, body):
    """Mirror the gateway's internal HMAC: hex(HMAC-SHA256(key, "domain:ts:" + body)).
    The full request body is signed so payload/tenant tampering invalidates the token.
    domain is "command" or "event"; body is the exact request bytes."""
    ts = str(int(time.time()))
    mac = hmac.new(INTERNAL_KEY.encode(), f"{domain}:{ts}:".encode() + body, hashlib.sha256)
    return mac.hexdigest(), ts


class OutpostSim:
    """Simulates an enrolled outpost dialing the gateway. user_id/org_id are the
    outpost's owning tenant (from the create response), needed to authorize
    internal commands targeting it."""

    def __init__(self, outpost_id, key, user_id="", org_id=""):
        self.outpost_id = outpost_id
        self.key = key
        self.user_id = user_id
        self.org_id = org_id

    @property
    def headers(self):
        return {"X-Outpost-ID": self.outpost_id, "Authorization": f"Bearer {self.key}"}

    def poll_commands(self, timeout=35):
        return requests.get(f"{GATEWAY_URL}/outpost/commands", headers=self.headers, timeout=timeout)

    def ack(self, command_id):
        return requests.post(f"{GATEWAY_URL}/outpost/commands/{command_id}/ack", headers=self.headers)

    def post_event(self, integration, type_, payload, event_id=None):
        body = {"integration": integration, "type": type_, "payload": payload}
        if event_id:
            body["event_id"] = event_id
        return requests.post(f"{GATEWAY_URL}/outpost/events", headers=self.headers, json=body)

    def heartbeat(self):
        return requests.post(f"{GATEWAY_URL}/outpost/heartbeat", headers=self.headers)


def register_outpost(bearer, name, modules):
    """Create an outpost via the user API and enroll it; returns OutpostSim."""
    res = requests.post(f"{GATEWAY_URL}/outposts", headers=bearer,
                        json={"name": name, "modules": modules})
    assert res.status_code == 201, f"create outpost failed: {res.text}"
    created = res.json()
    reg = requests.post(f"{GATEWAY_URL}/outpost/register",
                       json={"enrollment_token": created["enrollment_token"]})
    assert reg.status_code == 201, f"register failed: {reg.text}"
    enrolled = reg.json()
    return created["outpost_id"], OutpostSim(
        enrolled["outpost_id"], enrolled["outpost_key"],
        user_id=created.get("user_id", ""), org_id=created.get("org_id", ""),
    )


@pytest.fixture
def chaos_outpost(bearer):
    name = f"gw-chaos-{uuid.uuid4().hex[:6]}"
    return register_outpost(bearer, name, ["chaos"])
