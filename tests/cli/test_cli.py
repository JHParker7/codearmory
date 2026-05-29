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
