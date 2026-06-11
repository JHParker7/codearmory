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
    return engine.connect()


@pytest.fixture(scope="session")
def base_url():
    return os.getenv("CONDUCTOR_URL", "http://localhost:8082")


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

        result = conn.execute(
            sa.text("SELECT org_id FROM orgs WHERE owner_id = ANY(:uids)"),
            {"uids": user_ids},
        )
        org_ids = [str(r[0]) for r in result.fetchall()]

        conn.execute(
            sa.text("DELETE FROM invites WHERE inviter_id = ANY(:uids) OR invitee_email = ANY(:emails)"),
            {"uids": user_ids, "emails": user_emails},
        )

        conn.execute(sa.text("UPDATE users SET org_id = NULL WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("UPDATE users SET team_id = NULL WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("UPDATE teams SET owner_id = NULL WHERE owner_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("UPDATE orgs SET owner_id = NULL WHERE owner_id = ANY(:uids)"), {"uids": user_ids})

        if team_ids:
            conn.execute(sa.text("DELETE FROM teams WHERE team_id = ANY(:tids)"), {"tids": team_ids})
        if org_ids:
            conn.execute(sa.text("DELETE FROM orgs WHERE org_id = ANY(:oids)"), {"oids": org_ids})

        conn.execute(sa.text("DELETE FROM sessions WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("DELETE FROM permissions_checks WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        conn.execute(sa.text("DELETE FROM users WHERE user_id = ANY(:uids)"), {"uids": user_ids})
        all_role_ids = list(set(role_ids + team_role_ids))
        if all_role_ids:
            conn.execute(sa.text("DELETE FROM roles WHERE role_id = ANY(:rids)"), {"rids": all_role_ids})
        if perm_ids:
            conn.execute(sa.text("DELETE FROM permissions WHERE permissions_id = ANY(:pids)"), {"pids": perm_ids})

        conn.commit()
    except Exception as e:
        print(f"Cleanup error: {e}")
    finally:
        conn.close()


@pytest.fixture
def new_user(base_url):
    uid = uuid.uuid4().hex[:8]
    payload = {
        "email": f"test_{uid}@example.com",
        "username": f"user_{uid}",
        "password": "password123",
    }
    resp = requests.post(f"{base_url}/gatekeeper/signup", json=payload)
    assert resp.status_code == 201, f"fixture setup failed: {resp.text}"
    payload["user_id"] = resp.json()["user_id"]
    return payload


@pytest.fixture
def token(base_url, new_user):
    resp = requests.post(
        f"{base_url}/gatekeeper/login",
        json={"email": new_user["email"], "password": new_user["password"]},
    )
    assert resp.status_code == 200, f"token fixture login failed: {resp.text}"
    return resp.json()["token"]
