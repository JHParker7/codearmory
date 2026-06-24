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


def bust_user_cache(user_id: str) -> None:
    """Drop gatekeeper's cached user row after assigning role_id via raw SQL.

    Fixtures that set users.role_id directly in the DB bypass gatekeeper's Redis
    cache invalidation, leaving a stale gk:user:<id> holding the personal role
    minted at signup. A later handleCreateOrg loads that stale row via the cache
    and writes the whole row back, reverting role_id and orphaning the role we
    assigned (so the wildcard grant is lost and permission checks 403). Deleting
    the key forces gatekeeper to re-read the fresh role_id from the DB. No-op when
    redis is unavailable (cache disabled -> no staleness to fix).
    """
    url = os.getenv("REDIS_URL", "redis://localhost:6379/0")
    try:
        import redis  # imported lazily so the dep is only needed when caching is on

        client = redis.from_url(url)
        client.delete(f"gk:user:{user_id}")
        client.close()
    except Exception as e:  # best-effort: never fail a test on cache cleanup
        print(f"bust_user_cache({user_id}) skipped: {e}")


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

        # Step 1: delete leaf rows that foreign-key into sessions/permissions_checks,
        # then commit so this cleanup survives even if later deletes fail.
        conn.execute(sa.text("DELETE FROM sessions WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("DELETE FROM permissions_checks WHERE user_id = ANY(:uids)"), {"uids": user_ids})

        # MFA/OAuth rows have no FK to users, so they are never cascade-deleted and
        # would otherwise leak across test runs. totp_credentials/mfa_pending/oauth_codes
        # all carry a user_id column; delete by it. oauth_clients has no user_id (it is
        # only linked by name), so delete the clients the MFA tests create by name.
        conn.execute(sa.text("DELETE FROM totp_credentials WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("DELETE FROM mfa_pending WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("DELETE FROM oauth_codes WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text(
            "DELETE FROM oauth_clients WHERE name LIKE 'mfatest-%' OR name LIKE 'mfa-test-%'"
        ))
        if team_ids:
            conn.execute(sa.text("DELETE FROM permissions_checks WHERE team_id = ANY(:tids)"), {"tids": team_ids})
        if org_ids:
            conn.execute(sa.text("DELETE FROM permissions_checks WHERE org_id = ANY(:oids)"), {"oids": org_ids})
        conn.commit()

        # Step 2: null out FK columns before deleting parents (users → orgs → teams → roles)
        # to avoid FK constraint violations. Order matters: memberships first, then owners.
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

    # The raw-SQL role assignment above bypasses gatekeeper's cache invalidation;
    # drop the stale cached user row so the new role_id is honoured.
    bust_user_cache(user_id)

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
