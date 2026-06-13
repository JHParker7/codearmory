"""End-to-end CLI tests: full user journeys using the armory binary.

All commands run against a live Conductor service. The binary is compiled
once per session (or taken from ARMORY_BIN). CODEARMORY_URL is injected so
the binary targets the local Docker stack rather than its built-in default.

Coverage:
  - Auth lifecycle: signup, login, logout, status
  - Tickets CRUD: create, list, get, update, delete, comment
  - CI pipelines: create step/pipeline, trigger run, list/get run, cancel
  - Cross-service: create a run then link it to a ticket; full journey test
"""

import json
import uuid

import pytest
import requests

from conftest import API_URL


# ---------------------------------------------------------------------------
# Auth lifecycle
# ---------------------------------------------------------------------------

class TestAuthLifecycle:
    def test_signup_returns_user_id(self, run_cli):
        uid = uuid.uuid4().hex[:8]
        out, err, rc = run_cli(
            "auth", "signup",
            "--email", f"e2e_cli_{uid}@example.com",
            "--username", f"e2e_cli_{uid}",
            "--password", "clipass123",
        )
        assert rc == 0, f"stderr: {err}"
        body = json.loads(out)
        assert "user_id" in body

    def test_login_prints_logged_in(self, run_cli, new_user):
        out, err, rc = run_cli(
            "auth", "login",
            "--email", new_user["email"],
            "--password", new_user["password"],
        )
        assert rc == 0, f"stderr: {err}"
        # The CLI prints "Logged in — ..." to stderr (auth.go), not stdout.
        assert "Logged in" in err

    def test_status_with_token(self, run_cli, token):
        out, _, rc = run_cli("auth", "status", token=token)
        assert rc == 0
        assert "CODEARMORY_TOKEN" in out

    def test_logout_succeeds(self, run_cli):
        out, _, rc = run_cli("auth", "logout")
        assert rc == 0
        assert "Logged out" in out

    def test_duplicate_signup_fails(self, run_cli, new_user):
        _, _, rc = run_cli(
            "auth", "signup",
            "--email", new_user["email"],
            "--username", new_user["username"],
            "--password", "password123",
        )
        assert rc != 0

    def test_wrong_password_fails(self, run_cli, new_user):
        _, _, rc = run_cli(
            "auth", "login",
            "--email", new_user["email"],
            "--password", "definitely_wrong",
        )
        assert rc != 0

    def test_missing_email_fails(self, run_cli):
        _, _, rc = run_cli("auth", "login", "--password", "password123")
        assert rc != 0


# ---------------------------------------------------------------------------
# Tickets CRUD via CLI
# ---------------------------------------------------------------------------

class TestTicketsCLI:
    def test_create_ticket(self, run_cli, token):
        out, err, rc = run_cli(
            "tickets", "create",
            "--title", "CLI created ticket",
            "--priority", "high",
            token=token,
        )
        assert rc == 0, f"stderr: {err}"
        t = json.loads(out)
        assert t["title"] == "CLI created ticket"
        assert t["priority"] == "high"
        assert t["status"] == "open"

    def test_list_tickets_returns_array(self, run_cli, token):
        run_cli("tickets", "create", "--title", "List target", token=token)
        out, err, rc = run_cli("tickets", "list", token=token)
        assert rc == 0, f"stderr: {err}"
        tickets = json.loads(out)
        assert isinstance(tickets, list)
        titles = [t["title"] for t in tickets]
        assert "List target" in titles

    def test_get_ticket(self, run_cli, token):
        create_out, _, rc = run_cli(
            "tickets", "create", "--title", "Get target", token=token,
        )
        assert rc == 0
        tid = json.loads(create_out)["ticket_id"]

        out, err, rc = run_cli("tickets", "get", tid, token=token)
        assert rc == 0, f"stderr: {err}"
        assert json.loads(out)["ticket_id"] == tid

    def test_update_status(self, run_cli, token):
        create_out, _, rc = run_cli(
            "tickets", "create", "--title", "Update status target", token=token,
        )
        assert rc == 0
        tid = json.loads(create_out)["ticket_id"]

        out, err, rc = run_cli(
            "tickets", "update", tid, "--status", "in_progress", token=token,
        )
        assert rc == 0, f"stderr: {err}"
        assert json.loads(out)["status"] == "in_progress"

    def test_update_priority(self, run_cli, token):
        create_out, _, _ = run_cli(
            "tickets", "create", "--title", "Prio target", "--priority", "low",
            token=token,
        )
        tid = json.loads(create_out)["ticket_id"]

        out, err, rc = run_cli(
            "tickets", "update", tid, "--priority", "critical", token=token,
        )
        assert rc == 0, f"stderr: {err}"
        assert json.loads(out)["priority"] == "critical"

    def test_delete_ticket(self, run_cli, token):
        create_out, _, rc = run_cli(
            "tickets", "create", "--title", "Delete target", token=token,
        )
        assert rc == 0
        tid = json.loads(create_out)["ticket_id"]

        _, err, rc = run_cli("tickets", "delete", tid, token=token)
        assert rc == 0, f"stderr: {err}"

        # verify gone
        _, _, rc = run_cli("tickets", "get", tid, token=token)
        assert rc != 0

    def test_add_comment(self, run_cli, token):
        create_out, _, rc = run_cli(
            "tickets", "create", "--title", "Comment target", token=token,
        )
        assert rc == 0
        tid = json.loads(create_out)["ticket_id"]

        out, err, rc = run_cli(
            "tickets", "comment", "add", tid,
            "--body", "E2E CLI comment",
            token=token,
        )
        assert rc == 0, f"stderr: {err}"

    def test_list_filter_by_status(self, run_cli, token):
        run_cli("tickets", "create", "--title", "Open status test", token=token)
        out, err, rc = run_cli("tickets", "list", "--status", "open", token=token)
        assert rc == 0, f"stderr: {err}"
        for t in json.loads(out):
            assert t["status"] == "open"

    def test_create_requires_auth(self, run_cli):
        _, _, rc = run_cli("tickets", "create", "--title", "Unauthorized")
        assert rc != 0

    def test_list_requires_auth(self, run_cli):
        _, _, rc = run_cli("tickets", "list")
        assert rc != 0

    def test_create_missing_title_fails(self, run_cli, token):
        _, _, rc = run_cli("tickets", "create", "--priority", "low", token=token)
        assert rc != 0


# ---------------------------------------------------------------------------
# CI pipeline via CLI
# ---------------------------------------------------------------------------

class TestCIPipelineCLI:
    def _make_step(self, run_cli, token, suffix=None):
        name = f"e2e-step-{suffix or uuid.uuid4().hex[:6]}"
        out, err, rc = run_cli(
            "ci", "create", "step", name,
            "--action", "http",
            "--with", '{"service":"gatekeeper","method":"GET","path":"/healthz","expected_status":200}',
            "--timeout", "15",
            token=token,
        )
        assert rc == 0, f"step create failed: {err}"
        return json.loads(out)

    def test_create_step(self, run_cli, token):
        step = self._make_step(run_cli, token)
        assert "step_id" in step
        assert step["action"] == "http"
        run_cli("ci", "delete", "step", step["step_id"], token=token)

    def test_list_steps_includes_created(self, run_cli, token):
        step = self._make_step(run_cli, token)
        sid = step["step_id"]

        out, err, rc = run_cli("ci", "list", "steps", token=token)
        assert rc == 0, f"stderr: {err}"
        ids = [s["step_id"] for s in json.loads(out)]
        assert sid in ids

        run_cli("ci", "delete", "step", sid, token=token)

    def test_get_step(self, run_cli, token):
        step = self._make_step(run_cli, token)
        sid = step["step_id"]

        out, err, rc = run_cli("ci", "get", "step", sid, token=token)
        assert rc == 0, f"stderr: {err}"
        assert json.loads(out)["step_id"] == sid

        run_cli("ci", "delete", "step", sid, token=token)

    def test_create_pipeline(self, run_cli, token):
        step = self._make_step(run_cli, token)
        step_name = step["name"]
        sid = step["step_id"]

        pipeline_name = f"e2e-pipeline-{uuid.uuid4().hex[:6]}"
        out, err, rc = run_cli(
            "ci", "create", "pipeline", pipeline_name, "main", step_name,
            token=token,
        )
        assert rc == 0, f"stderr: {err}"
        pipeline = json.loads(out)
        assert "workflow_id" in pipeline

        run_cli("ci", "delete", "pipeline", pipeline["workflow_id"], token=token)
        run_cli("ci", "delete", "step", sid, token=token)

    def test_list_pipelines_includes_created(self, run_cli, token):
        step = self._make_step(run_cli, token)
        step_name, sid = step["name"], step["step_id"]

        pipeline_name = f"e2e-list-pipeline-{uuid.uuid4().hex[:6]}"
        pipeline_out, _, rc = run_cli(
            "ci", "create", "pipeline", pipeline_name, "main", step_name,
            token=token,
        )
        assert rc == 0
        wf_id = json.loads(pipeline_out)["workflow_id"]

        out, err, rc = run_cli("ci", "list", "pipelines", token=token)
        assert rc == 0, f"stderr: {err}"
        ids = [p["workflow_id"] for p in json.loads(out)]
        assert wf_id in ids

        run_cli("ci", "delete", "pipeline", wf_id, token=token)
        run_cli("ci", "delete", "step", sid, token=token)

    def test_trigger_run(self, run_cli, token):
        step = self._make_step(run_cli, token)
        step_name, sid = step["name"], step["step_id"]

        pipeline_out, _, _ = run_cli(
            "ci", "create", "pipeline", f"e2e-run-pipeline-{uuid.uuid4().hex[:6]}",
            "main", step_name, token=token,
        )
        wf_id = json.loads(pipeline_out)["workflow_id"]

        out, err, rc = run_cli("ci", "run", "pipeline", wf_id, token=token)
        assert rc == 0, f"stderr: {err}"
        run = json.loads(out)
        assert "run_id" in run

        run_cli("ci", "cancel", "run", run["run_id"], token=token)
        run_cli("ci", "delete", "pipeline", wf_id, token=token)
        run_cli("ci", "delete", "step", sid, token=token)

    def test_get_run_detail(self, run_cli, token):
        step = self._make_step(run_cli, token)
        step_name, sid = step["name"], step["step_id"]

        pipeline_out, _, _ = run_cli(
            "ci", "create", "pipeline", f"e2e-detail-pipeline-{uuid.uuid4().hex[:6]}",
            "main", step_name, token=token,
        )
        wf_id = json.loads(pipeline_out)["workflow_id"]

        run_out, _, _ = run_cli("ci", "run", "pipeline", wf_id, token=token)
        run_id = json.loads(run_out)["run_id"]

        out, err, rc = run_cli("ci", "get", "run", run_id, token=token)
        assert rc == 0, f"stderr: {err}"
        detail = json.loads(out)
        assert detail["run_id"] == run_id
        assert "status" in detail

        run_cli("ci", "cancel", "run", run_id, token=token)
        run_cli("ci", "delete", "pipeline", wf_id, token=token)
        run_cli("ci", "delete", "step", sid, token=token)

    def test_list_runs_filtered_by_pipeline(self, run_cli, token):
        step = self._make_step(run_cli, token)
        step_name, sid = step["name"], step["step_id"]

        pipeline_out, _, _ = run_cli(
            "ci", "create", "pipeline", f"e2e-filter-pipeline-{uuid.uuid4().hex[:6]}",
            "main", step_name, token=token,
        )
        wf_id = json.loads(pipeline_out)["workflow_id"]

        run_out, _, _ = run_cli("ci", "run", "pipeline", wf_id, token=token)
        run_id = json.loads(run_out)["run_id"]

        out, err, rc = run_cli("ci", "list", "runs", "--pipeline", wf_id, token=token)
        assert rc == 0, f"stderr: {err}"
        run_ids = [r["run_id"] for r in json.loads(out)]
        assert run_id in run_ids

        run_cli("ci", "cancel", "run", run_id, token=token)
        run_cli("ci", "delete", "pipeline", wf_id, token=token)
        run_cli("ci", "delete", "step", sid, token=token)

    def test_list_steps_requires_auth(self, run_cli):
        _, _, rc = run_cli("ci", "list", "steps")
        assert rc != 0

    def test_list_pipelines_requires_auth(self, run_cli):
        _, _, rc = run_cli("ci", "list", "pipelines")
        assert rc != 0

    def test_list_runs_requires_auth(self, run_cli):
        _, _, rc = run_cli("ci", "list", "runs")
        assert rc != 0

    def test_get_nonexistent_run_fails(self, run_cli, token):
        _, _, rc = run_cli("ci", "get", "run", str(uuid.uuid4()), token=token)
        assert rc != 0


# ---------------------------------------------------------------------------
# Cross-service via CLI: link a CI run to a ticket
# ---------------------------------------------------------------------------

class TestCrossServiceCLI:
    def _setup_pipeline(self, run_cli, token):
        """Return (step_id, step_name, workflow_id)."""
        step_name = f"e2e-xsvc-step-{uuid.uuid4().hex[:6]}"
        step_out, _, rc = run_cli(
            "ci", "create", "step", step_name,
            "--action", "http",
            "--with", '{"service":"gatekeeper","method":"GET","path":"/healthz","expected_status":200}',
            "--timeout", "15",
            token=token,
        )
        assert rc == 0
        sid = json.loads(step_out)["step_id"]

        pipeline_name = f"e2e-xsvc-pipeline-{uuid.uuid4().hex[:6]}"
        pipeline_out, _, rc = run_cli(
            "ci", "create", "pipeline", pipeline_name, "main", step_name,
            token=token,
        )
        assert rc == 0
        wf_id = json.loads(pipeline_out)["workflow_id"]
        return sid, step_name, wf_id

    def test_create_ticket_linked_to_run(self, run_cli, token):
        """Create a pipeline run via CLI, then create a ticket with --run."""
        sid, _, wf_id = self._setup_pipeline(run_cli, token)

        run_out, _, rc = run_cli("ci", "run", "pipeline", wf_id, token=token)
        assert rc == 0
        run_id = json.loads(run_out)["run_id"]

        ticket_out, err, rc = run_cli(
            "tickets", "create",
            "--title", "CLI-linked ticket",
            "--run", run_id,
            "--priority", "high",
            token=token,
        )
        assert rc == 0, f"stderr: {err}"
        ticket = json.loads(ticket_out)
        assert ticket["run_id"] == run_id
        assert ticket["title"] == "CLI-linked ticket"

        # verify link survives a GET
        get_out, _, rc = run_cli("tickets", "get", ticket["ticket_id"], token=token)
        assert rc == 0
        assert json.loads(get_out)["run_id"] == run_id

        # cleanup
        run_cli("tickets", "delete", ticket["ticket_id"], token=token)
        run_cli("ci", "cancel", "run", run_id, token=token)
        run_cli("ci", "delete", "pipeline", wf_id, token=token)
        run_cli("ci", "delete", "step", sid, token=token)

    def test_full_cli_journey(self, run_cli, new_user):
        """
        Complete CLI journey:
          login → create pipeline → trigger run → link ticket →
          move ticket through all statuses → verify final state.
        """
        # Get token for subsequent CLI calls
        login_res = requests.post(
            f"{API_URL}/gatekeeper/login",
            json={"email": new_user["email"], "password": new_user["password"]},
        )
        assert login_res.status_code == 200, login_res.text
        tok = login_res.json()["token"]

        # Build pipeline
        step_name = f"journey-step-{uuid.uuid4().hex[:6]}"
        run_cli(
            "ci", "create", "step", step_name,
            "--action", "http",
            "--with", '{"service":"gatekeeper","method":"GET","path":"/healthz","expected_status":200}',
            "--timeout", "15",
            token=tok,
        )

        pipeline_name = f"journey-pipeline-{uuid.uuid4().hex[:6]}"
        pipeline_out, _, rc = run_cli(
            "ci", "create", "pipeline", pipeline_name, "main", step_name,
            token=tok,
        )
        assert rc == 0
        wf_id = json.loads(pipeline_out)["workflow_id"]

        # Trigger run
        run_out, _, rc = run_cli("ci", "run", "pipeline", wf_id, token=tok)
        assert rc == 0
        run_id = json.loads(run_out)["run_id"]

        # Create linked ticket
        ticket_out, _, rc = run_cli(
            "tickets", "create",
            "--title", f"Journey ticket {uuid.uuid4().hex[:6]}",
            "--run", run_id,
            "--priority", "critical",
            token=tok,
        )
        assert rc == 0
        ticket = json.loads(ticket_out)
        tid = ticket["ticket_id"]
        assert ticket["run_id"] == run_id

        # Move through all statuses
        for status in ("in_progress", "resolved", "closed"):
            out, err, rc = run_cli(
                "tickets", "update", tid, "--status", status, token=tok,
            )
            assert rc == 0, f"update to {status!r} failed: {err}"
            assert json.loads(out)["status"] == status

        # Verify final state
        get_out, _, rc = run_cli("tickets", "get", tid, token=tok)
        assert rc == 0
        final = json.loads(get_out)
        assert final["status"] == "closed"
        assert final["run_id"] == run_id

        # cleanup
        run_cli("tickets", "delete", tid, token=tok)
        run_cli("ci", "cancel", "run", run_id, token=tok)
        run_cli("ci", "delete", "pipeline", wf_id, token=tok)
