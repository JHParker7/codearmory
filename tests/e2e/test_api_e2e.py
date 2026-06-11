"""End-to-end API tests: full user journeys from signup to running workflows.

All requests flow through Conductor (API_URL) so routing, auth middleware,
and RBAC are exercised on every call.

URL structure through Conductor:
  Auth      POST /signup  POST /login  GET /users/{id}
  Tickets   /tickets/tickets  /tickets/tickets/{id}  /tickets/tickets/{id}/comments
            /tickets/field-defs
  Workflows /workflows/steps  /workflows/workflows  /workflows/workflows/{id}/runs
            /workflows/runs  /workflows/runs/{id}  /workflows/actions
"""

import time
import uuid

import pytest
import requests

from conftest import API_URL


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def poll_run(bearer, run_id, timeout=30):
    """Poll GET /workflows/runs/{id} until the run reaches a terminal state."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        res = requests.get(f"{API_URL}/workflows/runs/{run_id}", headers=bearer)
        assert res.status_code == 200, f"poll got {res.status_code}: {res.text}"
        data = res.json()
        if data["status"] in ("completed", "failed", "cancelled"):
            return data
        time.sleep(1)
    pytest.fail(f"run {run_id} did not complete within {timeout}s")


def create_ticket(bearer, title, **kwargs):
    res = requests.post(f"{API_URL}/tickets/tickets", headers=bearer,
                        json={"title": title, **kwargs})
    assert res.status_code == 201, res.text
    return res.json()


def create_healthz_step(bearer, suffix=None):
    name = f"e2e-step-{suffix or uuid.uuid4().hex[:6]}"
    res = requests.post(f"{API_URL}/workflows/steps", headers=bearer, json={
        "name": name,
        "action": "http",
        "with": {
            "service": "gatekeeper",
            "method": "GET",
            "path": "/healthz",
            "expected_status": 200,
        },
        "timeout": 15,
    })
    assert res.status_code == 201, res.text
    return res.json()


def create_pipeline(bearer, step_ids, name=None):
    res = requests.post(f"{API_URL}/workflows/workflows", headers=bearer, json={
        "name": name or f"e2e-pipeline-{uuid.uuid4().hex[:6]}",
        "steps": [{"step_id": sid} for sid in step_ids],
    })
    assert res.status_code == 201, res.text
    return res.json()


def trigger_run(bearer, workflow_id):
    res = requests.post(
        f"{API_URL}/workflows/workflows/{workflow_id}/runs",
        headers=bearer,
    )
    assert res.status_code == 201, res.text
    return res.json()


def cleanup(bearer, *, ticket_ids=(), run_ids=(), workflow_ids=(), step_ids=()):
    for tid in ticket_ids:
        requests.delete(f"{API_URL}/tickets/tickets/{tid}", headers=bearer)
    for rid in run_ids:
        requests.delete(f"{API_URL}/workflows/runs/{rid}", headers=bearer)
    for wid in workflow_ids:
        requests.delete(f"{API_URL}/workflows/workflows/{wid}", headers=bearer)
    for sid in step_ids:
        requests.delete(f"{API_URL}/workflows/steps/{sid}", headers=bearer)


# ---------------------------------------------------------------------------
# Signup and login
# ---------------------------------------------------------------------------

class TestSignupAndLogin:
    def test_signup_creates_user(self):
        uid = uuid.uuid4().hex[:8]
        res = requests.post(f"{API_URL}/gatekeeper/signup", json={
            "email": f"e2e_signup_{uid}@example.com",
            "username": f"e2e_signup_{uid}",
            "password": "password123",
        })
        assert res.status_code == 201
        body = res.json()
        assert "user_id" in body
        assert body["email"] == f"e2e_signup_{uid}@example.com"
        assert body["username"] == f"e2e_signup_{uid}"

    def test_login_returns_jwt(self, new_user):
        res = requests.post(f"{API_URL}/gatekeeper/login", json={
            "email": new_user["email"],
            "password": new_user["password"],
        })
        assert res.status_code == 200
        tok = res.json().get("token", "")
        # JWTs begin with base64url-encoded header "eyJ"
        assert tok.startswith("eyJ"), f"expected a JWT, got: {tok!r}"

    def test_jwt_authorises_profile_fetch(self, token, new_user):
        """A freshly-issued token must allow GET /users/{id} through conductor."""
        res = requests.get(
            f"{API_URL}/gatekeeper/users/{new_user['user_id']}",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert res.status_code == 200
        assert res.json()["user_id"] == new_user["user_id"]
        assert res.json()["email"] == new_user["email"]

    def test_missing_fields_rejected(self):
        res = requests.post(f"{API_URL}/gatekeeper/signup", json={"email": "x@example.com"})
        assert res.status_code == 400

    def test_duplicate_email_rejected(self, new_user):
        res = requests.post(f"{API_URL}/gatekeeper/signup", json={
            "email": new_user["email"],
            "username": f"other_{uuid.uuid4().hex[:6]}",
            "password": "password123",
        })
        assert res.status_code == 409

    def test_wrong_password_returns_401(self, new_user):
        res = requests.post(f"{API_URL}/gatekeeper/login", json={
            "email": new_user["email"],
            "password": "definitely_wrong",
        })
        assert res.status_code == 401

    def test_unauthenticated_request_returns_401(self):
        res = requests.get(f"{API_URL}/tickets/tickets")
        assert res.status_code == 401


# ---------------------------------------------------------------------------
# Ticket journey
# ---------------------------------------------------------------------------

class TestTicketJourney:
    def test_create_ticket(self, bearer):
        t = create_ticket(bearer, "My first ticket", description="E2E test")
        assert t["ticket_id"] != ""
        assert t["title"] == "My first ticket"
        assert t["status"] == "open"
        assert t["priority"] == "medium"
        assert isinstance(t["comments"], list)

    def test_created_ticket_appears_in_list(self, bearer):
        unique = f"List check {uuid.uuid4().hex[:6]}"
        create_ticket(bearer, unique)
        res = requests.get(f"{API_URL}/tickets/tickets", headers=bearer)
        assert res.status_code == 200
        titles = [t["title"] for t in res.json()]
        assert unique in titles

    def test_get_ticket_by_id(self, bearer):
        t = create_ticket(bearer, "Get by ID")
        res = requests.get(f"{API_URL}/tickets/tickets/{t['ticket_id']}", headers=bearer)
        assert res.status_code == 200
        assert res.json()["ticket_id"] == t["ticket_id"]
        assert isinstance(res.json()["comments"], list)

    def test_full_status_lifecycle(self, bearer):
        """open → in_progress → resolved → closed via PUT."""
        t = create_ticket(bearer, "Lifecycle ticket")
        tid, title = t["ticket_id"], t["title"]
        for status in ("in_progress", "resolved", "closed"):
            res = requests.put(
                f"{API_URL}/tickets/tickets/{tid}", headers=bearer,
                json={"title": title, "status": status},
            )
            assert res.status_code == 200, f"transition to {status!r} failed: {res.text}"
            assert res.json()["status"] == status

    def test_add_comment_and_retrieve(self, bearer):
        t = create_ticket(bearer, "Comment target")
        tid = t["ticket_id"]
        res = requests.post(
            f"{API_URL}/tickets/tickets/{tid}/comments",
            headers=bearer,
            json={"body": "E2E comment body"},
        )
        assert res.status_code in (200, 201), res.text

        detail = requests.get(f"{API_URL}/tickets/tickets/{tid}", headers=bearer)
        assert detail.status_code == 200
        comments = detail.json().get("comments", [])
        assert any(c["body"] == "E2E comment body" for c in comments)

    def test_update_preserves_unset_fields(self, bearer):
        """A minimal PUT (title + status only) must not clear priority or description."""
        t = create_ticket(bearer, "Preserve fields", priority="critical",
                          description="Do not clear me")
        tid, title = t["ticket_id"], t["title"]

        res = requests.put(
            f"{API_URL}/tickets/tickets/{tid}", headers=bearer,
            json={"title": title, "status": "in_progress"},
        )
        assert res.status_code == 200
        body = res.json()
        assert body["priority"] == "critical", "priority was cleared by minimal PUT"
        assert body["description"] == "Do not clear me", "description was cleared by minimal PUT"

    def test_delete_ticket(self, bearer):
        t = create_ticket(bearer, "Delete me")
        tid = t["ticket_id"]
        res = requests.delete(f"{API_URL}/tickets/tickets/{tid}", headers=bearer)
        assert res.status_code == 204
        res = requests.get(f"{API_URL}/tickets/tickets/{tid}", headers=bearer)
        assert res.status_code == 404

    def test_filter_by_status(self, bearer):
        create_ticket(bearer, "Open filter check")
        res = requests.get(f"{API_URL}/tickets/tickets?status=open", headers=bearer)
        assert res.status_code == 200
        for t in res.json():
            assert t["status"] == "open"

    def test_filter_by_priority(self, bearer):
        create_ticket(bearer, "Priority filter check", priority="critical")
        res = requests.get(f"{API_URL}/tickets/tickets?priority=critical", headers=bearer)
        assert res.status_code == 200
        for t in res.json():
            assert t["priority"] == "critical"

    def test_field_defs_endpoint_reachable(self, bearer):
        res = requests.get(f"{API_URL}/tickets/field-defs", headers=bearer)
        assert res.status_code == 200
        assert isinstance(res.json(), list)


# ---------------------------------------------------------------------------
# Workflow journey
# ---------------------------------------------------------------------------

class TestWorkflowJourney:
    def test_create_step(self, bearer):
        step = create_healthz_step(bearer)
        assert "step_id" in step
        assert step["action"] == "http"
        requests.delete(f"{API_URL}/workflows/steps/{step['step_id']}", headers=bearer)

    def test_create_pipeline(self, bearer):
        step = create_healthz_step(bearer)
        pipeline = create_pipeline(bearer, [step["step_id"]])
        assert "workflow_id" in pipeline
        cleanup(bearer, workflow_ids=[pipeline["workflow_id"]], step_ids=[step["step_id"]])

    def test_list_pipelines_includes_created(self, bearer):
        step = create_healthz_step(bearer)
        pipeline = create_pipeline(bearer, [step["step_id"]])
        wf_id = pipeline["workflow_id"]

        res = requests.get(f"{API_URL}/workflows/workflows", headers=bearer)
        assert res.status_code == 200
        ids = [w["workflow_id"] for w in res.json()]
        assert wf_id in ids

        cleanup(bearer, workflow_ids=[wf_id], step_ids=[step["step_id"]])

    def test_trigger_run(self, bearer):
        step = create_healthz_step(bearer)
        pipeline = create_pipeline(bearer, [step["step_id"]])
        wf_id = pipeline["workflow_id"]

        run = trigger_run(bearer, wf_id)
        assert "run_id" in run
        assert run["workflow_id"] == wf_id

        cleanup(bearer, run_ids=[run["run_id"]], workflow_ids=[wf_id],
                step_ids=[step["step_id"]])

    def test_run_appears_in_list(self, bearer):
        step = create_healthz_step(bearer)
        pipeline = create_pipeline(bearer, [step["step_id"]])
        wf_id = pipeline["workflow_id"]

        run = trigger_run(bearer, wf_id)
        run_id = run["run_id"]

        res = requests.get(f"{API_URL}/workflows/runs?workflow_id={wf_id}", headers=bearer)
        assert res.status_code == 200
        assert run_id in [r["run_id"] for r in res.json()]

        cleanup(bearer, run_ids=[run_id], workflow_ids=[wf_id],
                step_ids=[step["step_id"]])

    def test_get_run_detail(self, bearer):
        step = create_healthz_step(bearer)
        pipeline = create_pipeline(bearer, [step["step_id"]])
        wf_id = pipeline["workflow_id"]

        run = trigger_run(bearer, wf_id)
        run_id = run["run_id"]

        res = requests.get(f"{API_URL}/workflows/runs/{run_id}", headers=bearer)
        assert res.status_code == 200
        detail = res.json()
        assert detail["run_id"] == run_id
        assert "status" in detail
        assert detail["workflow_id"] == wf_id

        cleanup(bearer, run_ids=[run_id], workflow_ids=[wf_id],
                step_ids=[step["step_id"]])

    def test_healthz_run_completes(self, bearer):
        """A single healthz step should complete successfully within 30 s."""
        step = create_healthz_step(bearer)
        pipeline = create_pipeline(bearer, [step["step_id"]])
        wf_id = pipeline["workflow_id"]

        run = trigger_run(bearer, wf_id)
        run_id = run["run_id"]

        final = poll_run(bearer, run_id, timeout=30)
        assert final["status"] == "completed", f"unexpected terminal status: {final['status']}"

        cleanup(bearer, run_ids=[run_id], workflow_ids=[wf_id],
                step_ids=[step["step_id"]])

    def test_list_actions_endpoint(self, bearer):
        res = requests.get(f"{API_URL}/workflows/actions", headers=bearer)
        assert res.status_code == 200
        assert isinstance(res.json(), list)

    def test_unauthenticated_trigger_rejected(self):
        res = requests.post(
            f"{API_URL}/workflows/workflows/{uuid.uuid4()}/runs"
        )
        assert res.status_code == 401


# ---------------------------------------------------------------------------
# Cross-service linkage
# ---------------------------------------------------------------------------

class TestCrossServiceLinkage:
    """Data flowing across the tickets and workflows services."""

    def test_ticket_stores_run_id(self, bearer):
        step = create_healthz_step(bearer)
        pipeline = create_pipeline(bearer, [step["step_id"]])
        wf_id = pipeline["workflow_id"]
        run = trigger_run(bearer, wf_id)
        run_id = run["run_id"]

        t = create_ticket(bearer, "Run-linked ticket",
                          workflow_id=wf_id, run_id=run_id)
        assert t["workflow_id"] == wf_id
        assert t["run_id"] == run_id

        detail = requests.get(f"{API_URL}/tickets/tickets/{t['ticket_id']}", headers=bearer)
        assert detail.status_code == 200
        assert detail.json()["run_id"] == run_id

        cleanup(bearer, ticket_ids=[t["ticket_id"]], run_ids=[run_id],
                workflow_ids=[wf_id], step_ids=[step["step_id"]])

    def test_linked_ticket_appears_in_unfiltered_list(self, bearer):
        step = create_healthz_step(bearer)
        pipeline = create_pipeline(bearer, [step["step_id"]])
        wf_id = pipeline["workflow_id"]
        run = trigger_run(bearer, wf_id)
        run_id = run["run_id"]

        t = create_ticket(bearer, f"List-check {uuid.uuid4().hex[:6]}",
                          workflow_id=wf_id, run_id=run_id)
        tid = t["ticket_id"]

        res = requests.get(f"{API_URL}/tickets/tickets", headers=bearer)
        assert res.status_code == 200
        ids = {item["ticket_id"] for item in res.json()}
        assert tid in ids

        cleanup(bearer, ticket_ids=[tid], run_ids=[run_id],
                workflow_ids=[wf_id], step_ids=[step["step_id"]])

    def test_run_link_survives_status_update(self, bearer):
        """Moving a ticket through statuses must not clear the run_id link."""
        step = create_healthz_step(bearer)
        pipeline = create_pipeline(bearer, [step["step_id"]])
        wf_id = pipeline["workflow_id"]
        run = trigger_run(bearer, wf_id)
        run_id = run["run_id"]

        t = create_ticket(bearer, "Run-link preserved", run_id=run_id)
        tid, title = t["ticket_id"], t["title"]

        for status in ("in_progress", "resolved"):
            res = requests.put(
                f"{API_URL}/tickets/tickets/{tid}", headers=bearer,
                json={"title": title, "status": status},
            )
            assert res.status_code == 200
            assert res.json()["run_id"] == run_id, \
                f"run_id cleared after move to {status!r}"

        cleanup(bearer, ticket_ids=[tid], run_ids=[run_id],
                workflow_ids=[wf_id], step_ids=[step["step_id"]])

    def test_full_signup_to_closed_ticket_journey(self):
        """
        Complete cross-service journey:
          signup → login → create pipeline → trigger run →
          create ticket linked to run → move ticket through all statuses to closed.
        """
        uid = uuid.uuid4().hex[:8]

        # 1. Signup
        signup = requests.post(f"{API_URL}/gatekeeper/signup", json={
            "email": f"e2e_journey_{uid}@example.com",
            "username": f"e2e_journey_{uid}",
            "password": "journey_pass_123",
        })
        assert signup.status_code == 201, signup.text

        # 2. Login
        login = requests.post(f"{API_URL}/gatekeeper/login", json={
            "email": f"e2e_journey_{uid}@example.com",
            "password": "journey_pass_123",
        })
        assert login.status_code == 200, login.text
        auth = {"Authorization": f"Bearer {login.json()['token']}"}

        # 3. Create a pipeline with one healthz step
        step_res = requests.post(f"{API_URL}/workflows/steps", headers=auth, json={
            "name": f"journey-step-{uid}",
            "action": "http",
            "with": {"service": "gatekeeper", "method": "GET",
                     "path": "/healthz", "expected_status": 200},
            "timeout": 15,
        })
        assert step_res.status_code == 201, step_res.text
        step_id = step_res.json()["step_id"]

        pipeline_res = requests.post(f"{API_URL}/workflows/workflows", headers=auth, json={
            "name": f"journey-pipeline-{uid}",
            "steps": [{"step_id": step_id}],
        })
        assert pipeline_res.status_code == 201, pipeline_res.text
        wf_id = pipeline_res.json()["workflow_id"]

        # 4. Trigger a run
        run_res = requests.post(
            f"{API_URL}/workflows/workflows/{wf_id}/runs", headers=auth,
        )
        assert run_res.status_code == 201, run_res.text
        run_id = run_res.json()["run_id"]

        # 5. Create a ticket linked to the run
        ticket_res = requests.post(f"{API_URL}/tickets/tickets", headers=auth, json={
            "title": f"Journey ticket {uid}",
            "description": "Tracks the E2E journey run",
            "priority": "high",
            "workflow_id": wf_id,
            "run_id": run_id,
        })
        assert ticket_res.status_code == 201, ticket_res.text
        ticket = ticket_res.json()
        tid = ticket["ticket_id"]
        assert ticket["run_id"] == run_id
        assert ticket["workflow_id"] == wf_id

        # 6. Progress through all statuses to closed
        for status in ("in_progress", "resolved", "closed"):
            res = requests.put(
                f"{API_URL}/tickets/tickets/{tid}", headers=auth,
                json={"title": ticket["title"], "status": status},
            )
            assert res.status_code == 200, f"move to {status!r} failed: {res.text}"
            assert res.json()["status"] == status

        # 7. Verify final closed state with link intact
        final = requests.get(f"{API_URL}/tickets/tickets/{tid}", headers=auth)
        assert final.status_code == 200
        body = final.json()
        assert body["status"] == "closed"
        assert body["run_id"] == run_id
        assert body["workflow_id"] == wf_id

        cleanup(auth, ticket_ids=[tid], run_ids=[run_id],
                workflow_ids=[wf_id], step_ids=[step_id])
