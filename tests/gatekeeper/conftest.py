import json as _json
import os
import uuid
import pytest
import requests

import sqlalchemy as sa


def connect() -> sa.Connection:
    DATABASE_URL = os.getenv(
        "DATABASE_URL", "postgresql://postgres:postgres@127.0.0.1:5432/gatekeeper"
    ).replace("postgresql", "postgresql+psycopg2")
    engine = sa.create_engine(DATABASE_URL)
    conn = engine.connect()
    return conn


@pytest.fixture(scope="session")
def base_url():
    return os.getenv("API_URL", "http://localhost:8080")


@pytest.fixture(scope="session")
def service_key():
    """X-Service-Key header value for calling internal endpoints directly."""
    return os.getenv("GATEKEEPER_SERVICE_KEY", "test-service:test-service-local-secret")


def pytest_sessionfinish(session, exitstatus):
    conn = None
    try:
        conn = connect()

        result = conn.execute(sa.text(
            "SELECT user_id, email, role_id FROM users WHERE email LIKE '%@example.com'"
        ))
        rows = result.fetchall()
        if not rows:
            return

        user_ids = [str(r[0]) for r in rows]
        user_emails = [str(r[1]) for r in rows]
        role_ids = [str(r[2]) for r in rows if r[2] is not None]

        perm_ids = []
        if role_ids:
            result = conn.execute(
                sa.text("SELECT permissions_ids FROM roles WHERE role_id = ANY(:rids) AND permissions_ids IS NOT NULL"),
                {"rids": role_ids},
            )
            for row in result:
                if row[0]:
                    perm_ids.extend([str(p) for p in row[0]])

        # Collect teams owned by test users and their associated roles (created by handleCreateTeam)
        result = conn.execute(
            sa.text("SELECT team_id, role_id FROM teams WHERE owner_id = ANY(:uids)"),
            {"uids": user_ids},
        )
        team_rows = result.fetchall()
        team_ids = [str(r[0]) for r in team_rows]
        team_role_ids = [str(r[1]) for r in team_rows if r[1] is not None]

        if team_role_ids:
            result = conn.execute(
                sa.text("SELECT permissions_ids FROM roles WHERE role_id = ANY(:rids) AND permissions_ids IS NOT NULL"),
                {"rids": team_role_ids},
            )
            for row in result:
                if row[0]:
                    perm_ids.extend([str(p) for p in row[0]])

        # Collect orgs owned by test users
        result = conn.execute(
            sa.text("SELECT org_id FROM orgs WHERE owner_id = ANY(:uids)"),
            {"uids": user_ids},
        )
        org_ids = [str(r[0]) for r in result.fetchall()]

        # Delete invites sent by or addressed to test users
        conn.execute(
            sa.text("DELETE FROM invites WHERE inviter_id = ANY(:uids) OR invitee_email = ANY(:emails)"),
            {"uids": user_ids, "emails": user_emails},
        )

        # Step 1: delete all permissions_checks that reference test data, commit
        # immediately so this survives even if later deletes fail.
        conn.execute(sa.text("DELETE FROM sessions WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("DELETE FROM permissions_checks WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        if team_ids:
            conn.execute(sa.text("DELETE FROM permissions_checks WHERE team_id = ANY(:tids)"), {"tids": team_ids})
        if org_ids:
            conn.execute(sa.text("DELETE FROM permissions_checks WHERE org_id = ANY(:oids)"), {"oids": org_ids})
        conn.commit()

        # Step 2: clear membership / ownership FKs, then delete orgs and teams.
        conn.execute(sa.text("UPDATE users SET org_id = NULL WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("UPDATE users SET team_id = NULL WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("UPDATE teams SET owner_id = NULL WHERE owner_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("UPDATE orgs SET owner_id = NULL WHERE owner_id = ANY(:uids)"), {"uids": user_ids})
        if team_ids:
            conn.execute(sa.text("DELETE FROM teams WHERE team_id = ANY(:tids)"), {"tids": team_ids})
        if org_ids:
            conn.execute(sa.text("UPDATE roles SET org_id = NULL WHERE org_id = ANY(:oids)"), {"oids": org_ids})
            conn.execute(sa.text("UPDATE secrets SET active = false WHERE org_id = ANY(:oids)"), {"oids": org_ids})
            conn.execute(sa.text("DELETE FROM org_secret_providers WHERE org_id = ANY(:oids)"), {"oids": org_ids})
            conn.execute(sa.text("DELETE FROM orgs WHERE org_id = ANY(:oids)"), {"oids": org_ids})

        conn.execute(
            sa.text("UPDATE service_permission_requests SET resolved_by = NULL WHERE resolved_by = ANY(:uids)"),
            {"uids": user_ids},
        )
        conn.execute(sa.text("DELETE FROM users WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        all_role_ids = list(set(role_ids + team_role_ids))
        if all_role_ids:
            conn.execute(sa.text("DELETE FROM roles WHERE role_id = ANY(:rids)"), {"rids": all_role_ids})
        if perm_ids:
            conn.execute(sa.text("DELETE FROM permissions WHERE permissions_id = ANY(:pids)"), {"pids": perm_ids})

        # Clean up service permission requests created by the test service accounts.
        conn.execute(
            sa.text("UPDATE service_permission_requests SET active = false WHERE service_name = ANY(:names)"),
            {"names": ["blueprints", "forge"]},
        )

        conn.commit()
    except Exception as e:
        print(f"Cleanup error: {e}")
    finally:
        if conn is not None:
            conn.close()


@pytest.fixture(scope="session")
def admin_token(base_url):
    """Admin user seeded with collection-level create permissions for all resources.

    Signup is done via the API so bcrypt hashing and session management work normally.
    The role with broad create permissions is inserted directly via the DB because no
    public endpoint can bootstrap admin access.
    """
    uid = uuid.uuid4().hex[:8]
    email = f"admin_{uid}@example.com"
    password = "adminpass123"

    resp = requests.post(
        f"{base_url}/signup",
        json={"email": email, "username": f"admin_{uid}", "password": password},
    )
    assert resp.status_code == 201, f"admin signup failed: {resp.text}"
    user_id = resp.json()["user_id"]

    conn = connect()
    perm_id = str(uuid.uuid4())
    role_id = str(uuid.uuid4())

    conn.execute(
        sa.text("""
            INSERT INTO permissions
                (permissions_id, name, service, actions, resources, active, created_at, updated_at)
            VALUES (
                :pid, 'admin-create-perms', 'gatekeeper',
                '["*"]'::jsonb,
                '["*"]'::jsonb,
                true, NOW(), NOW()
            )
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

    login = requests.post(
        f"{base_url}/login", json={"email": email, "password": password}
    )
    assert login.status_code == 200, f"admin login failed: {login.text}"

    return {
        "token": login.json()["token"],
        "user_id": user_id,
        "role_id": role_id,
        "email": email,
        "password": password,
    }


@pytest.fixture
def new_user(base_url):
    """Create a unique user via the API and return the payload plus the created user_id."""
    uid = uuid.uuid4().hex[:8]
    payload = {
        "email": f"test_{uid}@example.com",
        "username": f"user_{uid}",
        "password": "password123",
        "firstname": "Test",
        "lastname": "User",
    }
    resp = requests.post(f"{base_url}/signup", json=payload)
    assert resp.status_code == 201, f"fixture setup failed: {resp.text}"
    payload["user_id"] = resp.json()["user_id"]
    return payload


@pytest.fixture
def token(base_url, new_user):
    """Log in as new_user and return the JWT."""
    resp = requests.post(
        f"{base_url}/login",
        json={"email": new_user["email"], "password": new_user["password"]},
    )
    assert resp.status_code == 200, f"token fixture login failed: {resp.text}"
    return resp.json()["token"]


@pytest.fixture
def base_url_delete_fixture(base_url):
    """Create a throwaway user, log in, and return (token, user_id, email).

    Used by deletion tests so they don't share state with new_user/token.
    """
    uid = uuid.uuid4().hex[:8]
    email = f"del_{uid}@example.com"
    payload = {
        "email": email,
        "username": f"del_{uid}",
        "password": "password123",
    }
    resp = requests.post(f"{base_url}/signup", json=payload)
    assert resp.status_code == 201, f"delete fixture signup failed: {resp.text}"
    user_id = resp.json()["user_id"]

    login = requests.post(
        f"{base_url}/login", json={"email": email, "password": "password123"}
    )
    assert login.status_code == 200, f"delete fixture login failed: {login.text}"
    return login.json()["token"], user_id, email
