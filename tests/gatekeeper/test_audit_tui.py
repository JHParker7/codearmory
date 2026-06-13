"""Integration tests for the audit log API shapes that the audit TUI depends on."""
import requests


# ── Authentication ────────────────────────────────────────────────────────────

def test_list_audit_logs_unauthorized(base_url):
    res = requests.get(f"{base_url}/audit-logs")
    assert res.status_code == 401


# ── Response shape ────────────────────────────────────────────────────────────

def test_list_audit_logs_returns_array(base_url, admin_token):
    headers = {"Authorization": f"Bearer {admin_token['token']}"}
    res = requests.get(f"{base_url}/audit-logs", headers=headers)
    assert res.status_code == 200
    assert isinstance(res.json(), list)


def test_list_audit_logs_item_has_required_fields(base_url, admin_token):
    """The TUI reads audit_log_id, actor_id, actor_type, action, resource_id,
    detail, created_at from each entry."""
    headers = {"Authorization": f"Bearer {admin_token['token']}"}

    # Trigger an auditable action (login generates an audit log entry).
    requests.post(f"{base_url}/login", json={
        "email": admin_token["email"],
        "password": admin_token["password"],
    })

    res = requests.get(f"{base_url}/audit-logs", headers=headers)
    assert res.status_code == 200
    items = res.json()
    # The login above (plus the admin fixture's own login) guarantees a
    # session.create entry, so the field-shape assertions below always run.
    assert items, "expected at least one audit log entry after an auditable action"

    item = items[0]
    for field in ("audit_log_id", "actor_id", "actor_type", "action", "created_at"):
        assert field in item, f"audit log item missing field: {field}"
    # resource_id and detail may be empty strings but the key must exist
    assert "resource_id" in item
    assert "detail" in item


def test_list_audit_logs_created_at_is_string(base_url, admin_token):
    """created_at must be a string so the TUI can parse it as time.Time."""
    headers = {"Authorization": f"Bearer {admin_token['token']}"}
    res = requests.get(f"{base_url}/audit-logs", headers=headers)
    assert res.status_code == 200
    items = res.json()
    for item in items[:5]:  # spot-check first 5
        assert isinstance(item["created_at"], str), (
            f"created_at should be a string, got: {type(item['created_at'])}"
        )


# ── Pagination ────────────────────────────────────────────────────────────────

def test_list_audit_logs_accepts_limit_and_offset(base_url, admin_token):
    """The TUI sends ?limit=50&offset=N — server must accept these params."""
    headers = {"Authorization": f"Bearer {admin_token['token']}"}
    res = requests.get(f"{base_url}/audit-logs", headers=headers,
                       params={"limit": 50, "offset": 0})
    assert res.status_code == 200
    assert isinstance(res.json(), list)


def test_list_audit_logs_offset_beyond_end_returns_empty(base_url, admin_token):
    """A large offset should return an empty array, not an error."""
    headers = {"Authorization": f"Bearer {admin_token['token']}"}
    res = requests.get(f"{base_url}/audit-logs", headers=headers,
                       params={"limit": 50, "offset": 999999})
    assert res.status_code == 200
    assert res.json() == []


def test_list_audit_logs_limit_respected(base_url, admin_token):
    """Requesting limit=1 should return at most 1 entry."""
    headers = {"Authorization": f"Bearer {admin_token['token']}"}
    res = requests.get(f"{base_url}/audit-logs", headers=headers,
                       params={"limit": 1, "offset": 0})
    assert res.status_code == 200
    assert len(res.json()) <= 1


# ── Access control: listing audit logs requires the listAuditLog permission ───

def test_regular_user_forbidden_from_audit_logs(base_url, token):
    """handleListAuditLogs does no per-actor scoping — any holder of the
    listAuditLog permission sees every actor's entries. This test only verifies
    the access gate: a regular user without that permission is forbidden (403)."""
    headers = {"Authorization": f"Bearer {token}"}
    res = requests.get(f"{base_url}/audit-logs", headers=headers)
    assert res.status_code == 403
