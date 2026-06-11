"""Integration tests for the armory CLI tool.

These tests invoke the compiled armory binary against a running conductor
service. All tests that need authentication create their own user via signup;
no shared mutable state is assumed between tests.
"""

import json
import os
import tempfile
import uuid

import requests


# ---------------------------------------------------------------------------
# Help / unknown commands
# ---------------------------------------------------------------------------


class TestHelp:
    def test_help_exits_zero(self, run_cli):
        _, _, rc = run_cli("--help")
        assert rc == 0

    def test_help_mentions_armory(self, run_cli):
        out, _, _ = run_cli("--help")
        assert "armory" in out

    def test_unknown_command_exits_nonzero(self, run_cli):
        _, _, rc = run_cli("notacommand")
        assert rc != 0

    def test_unknown_command_prints_error(self, run_cli):
        _, err, _ = run_cli("notacommand")
        assert "unknown command" in err


# ---------------------------------------------------------------------------
# Auth: signup
# ---------------------------------------------------------------------------


class TestSignup:
    def test_signup_exits_zero(self, run_cli, conductor_url):
        uid = uuid.uuid4().hex[:8]
        out, err, rc = run_cli(
            "auth", "signup",
            "--email", f"signup_{uid}@example.com",
            "--username", f"signup_{uid}",
            "--password", "password123",
        )
        assert rc == 0, f"stderr: {err}"

    def test_signup_returns_json(self, run_cli, conductor_url):
        uid = uuid.uuid4().hex[:8]
        out, _, rc = run_cli(
            "auth", "signup",
            "--email", f"signup_{uid}@example.com",
            "--username", f"signup_{uid}",
            "--password", "password123",
        )
        assert rc == 0
        body = json.loads(out)
        assert "user_id" in body

    def test_signup_duplicate_exits_nonzero(self, run_cli, new_user):
        _, _, rc = run_cli(
            "auth", "signup",
            "--email", new_user["email"],
            "--username", new_user["username"],
            "--password", "password123",
        )
        assert rc != 0

    def test_signup_missing_email_exits_nonzero(self, run_cli):
        _, _, rc = run_cli("auth", "signup", "--username", "x", "--password", "password123")
        assert rc != 0

    def test_signup_missing_password_exits_nonzero(self, run_cli):
        _, _, rc = run_cli("auth", "signup", "--email", "a@example.com", "--username", "x")
        assert rc != 0


# ---------------------------------------------------------------------------
# Auth: login
# ---------------------------------------------------------------------------


class TestLogin:
    def test_login_exits_zero(self, run_cli, new_user):
        _, err, rc = run_cli(
            "auth", "login",
            "--email", new_user["email"],
            "--password", new_user["password"],
        )
        assert rc == 0, f"stderr: {err}"

    def test_login_prints_logged_in(self, run_cli, new_user):
        out, _, _ = run_cli(
            "auth", "login",
            "--email", new_user["email"],
            "--password", new_user["password"],
        )
        assert "Logged in" in out

    def test_login_wrong_password_exits_nonzero(self, run_cli, new_user):
        _, _, rc = run_cli(
            "auth", "login",
            "--email", new_user["email"],
            "--password", "wrongpass",
        )
        assert rc != 0

    def test_login_unknown_email_exits_nonzero(self, run_cli):
        _, _, rc = run_cli(
            "auth", "login",
            "--email", f"nobody_{uuid.uuid4().hex}@example.com",
            "--password", "password123",
        )
        assert rc != 0

    def test_login_missing_email_exits_nonzero(self, run_cli):
        _, _, rc = run_cli("auth", "login", "--password", "password123")
        assert rc != 0

    def test_login_missing_password_exits_nonzero(self, run_cli):
        _, _, rc = run_cli("auth", "login", "--email", "a@example.com")
        assert rc != 0


# ---------------------------------------------------------------------------
# Auth: logout
# ---------------------------------------------------------------------------


class TestLogout:
    def test_logout_exits_zero(self, run_cli):
        out, _, rc = run_cli("auth", "logout")
        assert rc == 0
        assert "Logged out" in out


# ---------------------------------------------------------------------------
# Auth: status
# ---------------------------------------------------------------------------


class TestAuthStatus:
    def test_status_exits_zero(self, run_cli, token):
        _, _, rc = run_cli("auth", "status", token=token)
        assert rc == 0

    def test_status_shows_url(self, run_cli, conductor_url, token):
        out, _, _ = run_cli("auth", "status", token=token)
        assert conductor_url in out

    def test_status_env_token_shows_source(self, run_cli, token):
        out, _, _ = run_cli("auth", "status", token=token)
        assert "CODEARMORY_TOKEN" in out

    def test_status_no_token_shows_not_set(self, run_cli):
        out, _, rc = run_cli("auth", "status")
        assert rc == 0
        assert "not set" in out

    def test_status_flag_token_shows_source(self, run_cli, token):
        out, _, _ = run_cli("auth", "status", "--token", token)
        assert "--token flag" in out


# ---------------------------------------------------------------------------
# Token source precedence
# ---------------------------------------------------------------------------


class TestTokenPrecedence:
    def test_flag_overrides_env(self, run_cli, token):
        # Both token (env) and --token flag are set; flag should win.
        # We verify by checking status output label, not by calling an API.
        out, _, rc = run_cli("auth", "status", "--token", token, token="other-value")
        assert rc == 0
        assert "--token flag" in out

    def test_env_token_is_sent_as_bearer(self, run_cli, token, new_user):
        # Fetching the user's own record using the env token should succeed.
        out, err, rc = run_cli("users", "get", new_user["user_id"], token=token)
        assert rc == 0, f"stderr: {err}"

    def test_no_token_request_still_sent(self, run_cli):
        # With no token the request is still made (no auth header); conductor
        # will reject with 401. The CLI should exit non-zero.
        _, _, rc = run_cli("users", "list")
        assert rc != 0


# ---------------------------------------------------------------------------
# Users
# ---------------------------------------------------------------------------


class TestUsers:
    def test_get_own_user_exits_zero(self, run_cli, token, new_user):
        out, err, rc = run_cli("users", "get", new_user["user_id"], token=token)
        assert rc == 0, f"stderr: {err}"

    def test_get_own_user_returns_json(self, run_cli, token, new_user):
        out, _, rc = run_cli("users", "get", new_user["user_id"], token=token)
        assert rc == 0
        body = json.loads(out)
        assert body.get("user_id") == new_user["user_id"]

    def test_get_nonexistent_user_exits_nonzero(self, run_cli, token):
        _, _, rc = run_cli("users", "get", str(uuid.uuid4()), token=token)
        assert rc != 0

    def test_update_own_user_exits_zero(self, run_cli, token, new_user):
        out, err, rc = run_cli(
            "users", "update", new_user["user_id"],
            "--data", json.dumps({"email": new_user["email"], "username": new_user["username"], "firstname": "Updated"}),
            token=token,
        )
        assert rc == 0, f"stderr: {err}"

    def test_update_invalid_json_exits_nonzero(self, run_cli, token, new_user):
        _, _, rc = run_cli(
            "users", "update", new_user["user_id"],
            "--data", "not-json",
            token=token,
        )
        assert rc != 0

    def test_update_from_file(self, run_cli, token, new_user):
        f = tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False)
        json.dump({"email": new_user["email"], "username": new_user["username"], "firstname": "FromFile"}, f)
        f.close()
        try:
            out, err, rc = run_cli(
                "users", "update", new_user["user_id"],
                "--data", f"@{f.name}",
                token=token,
            )
            assert rc == 0, f"stderr: {err}"
        finally:
            os.unlink(f.name)


# ---------------------------------------------------------------------------
# State: user-scoped
# ---------------------------------------------------------------------------


class TestUserScopedState:
    def _workspace(self):
        return f"cli-test-{uuid.uuid4().hex[:8]}"

    def test_push_and_get_state(self, run_cli, token, new_user):
        workspace = self._workspace()
        username = new_user["username"]
        state = json.dumps({"version": 4, "serial": 1, "lineage": str(uuid.uuid4()), "outputs": {}, "resources": []})

        _, err, rc = run_cli("state", "push", username, workspace, "--file", "-",
                             token=token, stdin=state)
        assert rc == 0, f"push failed: {err}"

        out, err, rc = run_cli("state", "get", username, workspace, token=token)
        assert rc == 0, f"get failed: {err}"
        body = json.loads(out)
        assert body["serial"] == 1

    def test_push_from_file(self, run_cli, token, new_user):
        workspace = self._workspace()
        username = new_user["username"]
        state = {"version": 4, "serial": 2, "lineage": str(uuid.uuid4()), "outputs": {}, "resources": []}

        f = tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False)
        json.dump(state, f)
        f.close()
        try:
            _, err, rc = run_cli("state", "push", username, workspace, "--file", f.name, token=token)
            assert rc == 0, f"stderr: {err}"
        finally:
            os.unlink(f.name)

    def test_delete_state(self, run_cli, token, new_user):
        workspace = self._workspace()
        username = new_user["username"]
        state = json.dumps({"version": 4, "serial": 1, "lineage": str(uuid.uuid4()), "outputs": {}, "resources": []})

        run_cli("state", "push", username, workspace, "--file", "-", token=token, stdin=state)
        _, err, rc = run_cli("state", "delete", username, workspace, token=token)
        assert rc == 0, f"delete failed: {err}"

    def test_lock_and_unlock(self, run_cli, token, new_user):
        workspace = self._workspace()
        username = new_user["username"]
        state = json.dumps({"version": 4, "serial": 1, "lineage": str(uuid.uuid4()), "outputs": {}, "resources": []})
        run_cli("state", "push", username, workspace, "--file", "-", token=token, stdin=state)

        lock_id = str(uuid.uuid4())
        lock_info = json.dumps({
            "ID": lock_id,
            "Operation": "OperationTypePlan",
            "Who": "cli-test@host",
            "Info": "",
            "Version": "1.5.0",
            "Created": "2024-01-01T00:00:00.000000000Z",
            "Path": "",
        })
        _, err, rc = run_cli("state", "lock", username, workspace, "--data", lock_info, token=token)
        assert rc == 0, f"lock failed: {err}"

        _, err, rc = run_cli("state", "unlock", username, workspace,
                             "--data", json.dumps({"ID": lock_id}), token=token)
        assert rc == 0, f"unlock failed: {err}"

    def test_get_missing_state_returns_success(self, run_cli, token, new_user):
        # Blueprints returns 204 No Content for an empty workspace — not an error.
        out, _, rc = run_cli("state", "get", new_user["username"], self._workspace(), token=token)
        assert rc == 0
        assert out.strip() == ""

    def test_push_requires_auth(self, run_cli, new_user):
        state = json.dumps({"version": 4, "serial": 1, "lineage": str(uuid.uuid4()), "outputs": {}, "resources": []})
        _, _, rc = run_cli("state", "push", new_user["username"], self._workspace(),
                           "--file", "-", stdin=state)
        assert rc != 0


# ---------------------------------------------------------------------------
# JSON output
# ---------------------------------------------------------------------------


class TestJSONOutput:
    def test_output_is_pretty_printed(self, run_cli, token, new_user):
        out, _, rc = run_cli("users", "get", new_user["user_id"], token=token)
        assert rc == 0
        assert "\n" in out

    def test_output_is_valid_json(self, run_cli, token, new_user):
        out, _, rc = run_cli("users", "get", new_user["user_id"], token=token)
        assert rc == 0
        json.loads(out)


# ---------------------------------------------------------------------------
# Error handling
# ---------------------------------------------------------------------------


class TestErrorHandling:
    def test_http_error_exits_nonzero(self, run_cli, token):
        _, err, rc = run_cli("users", "get", str(uuid.uuid4()), token=token)
        assert rc != 0

    def test_http_error_message_contains_status_code(self, run_cli, token):
        _, err, rc = run_cli("users", "get", str(uuid.uuid4()), token=token)
        assert rc != 0
        assert any(code in err for code in ["404", "403", "401", "HTTP"])

    def test_unreachable_server_exits_nonzero(self, run_cli):
        _, _, rc = run_cli(
            "--url", "http://127.0.0.1:1",
            "users", "list",
            extra_env={"CODEARMORY_TOKEN": "fake-token"},
        )
        assert rc != 0

    def test_unreachable_server_prints_error(self, run_cli):
        _, err, rc = run_cli(
            "--url", "http://127.0.0.1:1",
            "users", "list",
            extra_env={"CODEARMORY_TOKEN": "fake-token"},
        )
        assert rc != 0
        assert err.strip() != ""


# ---------------------------------------------------------------------------
# CI TUI — command registration and underlying API calls
# ---------------------------------------------------------------------------


class TestCITUI:
    """Verify the `ci tui` command is registered and its help text is coherent."""

    def test_ci_tui_help_exits_zero(self, run_cli):
        _, _, rc = run_cli("ci", "tui", "--help")
        assert rc == 0

    def test_ci_tui_help_mentions_browse(self, run_cli):
        out, _, _ = run_cli("ci", "tui", "--help")
        assert "browse" in out.lower() or "pipeline" in out.lower() or "interactive" in out.lower()

    def test_ci_tui_help_rejects_extra_args(self, run_cli):
        _, _, rc = run_cli("ci", "tui", "unexpected-arg")
        assert rc != 0


class TestCICmds:
    """Smoke-test the non-interactive CI commands that back the TUI views.

    These verify the full path from CLI binary → conductor → workflows service,
    covering the same endpoints the TUI calls when loading its views.
    """

    def test_list_pipelines_requires_auth(self, run_cli):
        _, _, rc = run_cli("ci", "list", "pipelines")
        assert rc != 0

    def test_list_pipelines_with_auth_exits_zero(self, run_cli, token):
        _, _, rc = run_cli("ci", "list", "pipelines", token=token)
        assert rc == 0

    def test_list_runs_requires_auth(self, run_cli):
        _, _, rc = run_cli("ci", "list", "runs")
        assert rc != 0

    def test_list_runs_with_auth_exits_zero(self, run_cli, token):
        _, _, rc = run_cli("ci", "list", "runs", token=token)
        assert rc == 0

    def test_list_steps_with_auth_exits_zero(self, run_cli, token):
        _, _, rc = run_cli("ci", "list", "steps", token=token)
        assert rc == 0

    def test_get_run_nonexistent_exits_nonzero(self, run_cli, token):
        import uuid as _uuid
        _, _, rc = run_cli("ci", "get", "run", str(_uuid.uuid4()), token=token)
        assert rc != 0

    def test_create_pipeline_lifecycle(self, run_cli, token):
        """Create a step, pipeline, trigger a run, then list runs to verify TUI data path."""
        import json as _json
        import uuid as _uuid

        step_name = f"tui-test-step-{_uuid.uuid4().hex[:8]}"

        # Create step.
        out, _, rc = run_cli(
            "ci", "create", "step", step_name,
            "--action", "http",
            "--with", '{"service":"gatekeeper","method":"GET","path":"/healthz"}',
            "--timeout", "10",
            token=token,
        )
        assert rc == 0, f"step create failed: {out}"
        step = _json.loads(out)
        step_id = step["step_id"]

        # Create pipeline referencing the step.
        pipeline_name = f"tui-test-pipeline-{_uuid.uuid4().hex[:8]}"
        out, _, rc = run_cli(
            "ci", "create", "pipeline", pipeline_name, "main", step_name,
            token=token,
        )
        assert rc == 0, f"pipeline create failed: {out}"
        pipeline = _json.loads(out)
        pipeline_id = pipeline["workflow_id"]

        # List pipelines — the new pipeline should appear.
        out, _, rc = run_cli("ci", "list", "pipelines", token=token)
        assert rc == 0
        pipelines = _json.loads(out)
        ids = [p["workflow_id"] for p in pipelines]
        assert pipeline_id in ids, f"pipeline {pipeline_id} not in list: {ids}"

        # Trigger a run.
        out, _, rc = run_cli("ci", "run", "pipeline", pipeline_id, token=token)
        assert rc == 0, f"run failed: {out}"
        run = _json.loads(out)
        run_id = run["run_id"]

        # List runs filtered to this pipeline — the run should be present.
        out, _, rc = run_cli(
            "ci", "list", "runs", "--pipeline", pipeline_id,
            token=token,
        )
        assert rc == 0
        runs = _json.loads(out)
        run_ids = [r["run_id"] for r in runs]
        assert run_id in run_ids, f"run {run_id} not in list: {run_ids}"

        # Get the run detail — this is the exact call the TUI's run-detail view makes.
        out, _, rc = run_cli("ci", "get", "run", run_id, token=token)
        assert rc == 0, f"get run failed: {out}"
        detail = _json.loads(out)
        assert detail["run_id"] == run_id

        # Cleanup.
        run_cli("ci", "cancel", "run", run_id, token=token)
        run_cli("ci", "delete", "pipeline", pipeline_id, token=token)
        run_cli("ci", "delete", "step", step_id, token=token)
