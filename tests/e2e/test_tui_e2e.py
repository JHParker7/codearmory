"""End-to-end TUI tests: drive the real `armory` TUI through a pseudo-terminal.

Unlike API-shape checks (those live in tests/<service>/test_*_tui.py), these
tests launch the compiled `armory` binary inside a PTY, send real keystrokes,
and assert on the frames bubbletea actually renders. The full path is exercised:

    keypress → bubbletea model → armory HTTP client → Conductor → backend

A pseudo-terminal is required because bubbletea only starts its event loop when
stdin/stdout are a TTY; pexpect provides one. The strongest test here
(`test_board_move_card_updates_api`) presses `L` to shift a card between
columns in the TUI and then confirms, over the API, that the move was persisted
— proving the keystroke travelled all the way to the backend.

TUI surfaces under test:
  Home   `armory`            home menu → opens sub-TUIs, returns home, quits
  Board  `armory tickets`    kanban columns, card move, auth-error screen
  CI     `armory ci tui`     pipeline list → drill into runs

Each test authenticates as a freshly signed-up user (the `token`/`bearer`
fixtures), so the board/pipeline lists contain only data this test seeds.
"""

import os
import re
import tempfile
import uuid

import pytest
import requests

from conftest import API_URL, CONDUCTOR_URL

pexpect = pytest.importorskip("pexpect")

# Matches the ANSI/VT escape sequences bubbletea emits (CSI, OSC, single-char).
_ANSI_PAT = r"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[\[\]()][0-9;?]*[ -/]*[@-~]|\x1b[@-Z\\-_]"
_ANSI_RE = re.compile(_ANSI_PAT)

# A PTY wide enough for four board columns (4 × ~30) and tall enough for a card.
_DIMENSIONS = (50, 220)


def _strip_ansi(s: str) -> str:
    return _ANSI_RE.sub("", s or "")


def _tolerant_pattern(text: str) -> str:
    """Build a regex for ``text`` that tolerates ANSI/CSI sequences (and the
    soft line-wraps a PTY inserts) appearing between any two characters.

    lipgloss emits cursor-move/erase CSI sequences mid-line, so a header like
    "In Progress (1)" can arrive on the raw pexpect stream with escape codes
    spliced between its characters. Matching the bare ``re.escape(text)`` then
    flakily fails. Allowing optional ANSI runs (and stray CR/LF from wrapping)
    between each character makes the match robust while keeping it literal.
    """
    sep = f"(?:{_ANSI_PAT}|[\r\n])*"
    return sep.join(re.escape(ch) for ch in text)


def _expect(child, *texts, timeout=20):
    """Assert each text appears, in order, on the TUI stream.

    Matching is done against an ANSI-tolerant view of the stream so cursor/erase
    CSI sequences that lipgloss splices into header text do not cause flakes.
    """
    for text in texts:
        try:
            child.expect(_tolerant_pattern(text), timeout=timeout)
        except (pexpect.TIMEOUT, pexpect.EOF) as exc:
            screen = _strip_ansi(child.before)[-2000:]
            raise AssertionError(
                f"expected {text!r} on the TUI screen but it never appeared.\n"
                f"--- last frame ---\n{screen}"
            ) from exc


def _expect_eof(child, timeout=10):
    try:
        child.expect(pexpect.EOF, timeout=timeout)
    except pexpect.TIMEOUT as exc:
        raise AssertionError("TUI did not exit after quit keystroke") from exc


@pytest.fixture
def tui(armory_bin):
    """Spawn the armory TUI in a PTY targeting the local stack.

    Returns a callable: tui("tickets", token=jwt) -> pexpect child. Children are
    force-closed on teardown so a hung TUI never leaks into the next test.
    """
    children = []

    def _spawn(*args, token=None, timeout=20):
        env = {
            "HOME": tempfile.mkdtemp(prefix="armory-tui-home-"),
            "PATH": os.environ.get("PATH", ""),
            "CODEARMORY_URL": CONDUCTOR_URL,
            "TERM": "xterm-256color",
            "NO_COLOR": "1",  # drop SGR color codes so rendered text stays contiguous
        }
        if token:
            env["CODEARMORY_TOKEN"] = token
        child = pexpect.spawn(
            armory_bin,
            list(args),
            env=env,
            encoding="utf-8",
            codec_errors="replace",
            timeout=timeout,
            dimensions=_DIMENSIONS,
        )
        children.append(child)
        return child

    yield _spawn

    for child in children:
        try:
            if child.isalive():
                child.sendcontrol("c")
                child.expect(pexpect.EOF, timeout=5)
        except Exception:
            pass
        finally:
            child.close(force=True)


# ---------------------------------------------------------------------------
# Seeding helpers (data the TUI then renders)
# ---------------------------------------------------------------------------

def create_ticket(bearer, title, **kwargs):
    res = requests.post(f"{API_URL}/tickets/tickets", headers=bearer,
                        json={"title": title, **kwargs})
    assert res.status_code == 201, res.text
    return res.json()


def create_healthz_step(bearer):
    res = requests.post(f"{API_URL}/workflows/steps", headers=bearer, json={
        "name": f"tui-step-{uuid.uuid4().hex[:6]}",
        "action": "http",
        "with": {"service": "gatekeeper", "method": "GET",
                 "path": "/healthz", "expected_status": 200},
        "timeout": 15,
    })
    assert res.status_code == 201, res.text
    return res.json()


def create_pipeline(bearer, step_ids, name):
    res = requests.post(f"{API_URL}/workflows/pipelines", headers=bearer, json={
        "name": name,
        "steps": [{"step_id": sid} for sid in step_ids],
    })
    assert res.status_code == 201, res.text
    return res.json()


def trigger_run(bearer, workflow_id):
    res = requests.post(f"{API_URL}/workflows/pipelines/{workflow_id}/runs",
                        headers=bearer)
    assert res.status_code == 201, res.text
    return res.json()


# ---------------------------------------------------------------------------
# Home TUI — bare `armory`
# ---------------------------------------------------------------------------

class TestHomeTUI:
    """The top-level menu rendered by running `armory` with no subcommand."""

    def test_home_renders_menu(self, tui, token):
        child = tui(token=token)
        _expect(child, "codearmory", "CI / Pipelines", "Tickets Board", "Audit Log")
        child.send("q")
        _expect_eof(child)

    def test_home_opens_board_then_returns_home(self, tui, token):
        """j to highlight 'Tickets Board', enter to open it, q to return home."""
        child = tui(token=token)
        _expect(child, "Tickets Board")
        child.send("j")        # move cursor: CI / Pipelines -> Tickets Board
        child.send("\r")       # enter: launch the board sub-TUI
        _expect(child, "Open")  # board column header only exists in the board view
        child.send("q")        # board 'q' returns to the home menu
        _expect(child, "codearmory")
        child.send("q")        # home 'q' quits the program
        _expect_eof(child)


# ---------------------------------------------------------------------------
# Board TUI — `armory tickets`
# ---------------------------------------------------------------------------

class TestBoardTUI:
    """The kanban board, driven directly via `armory tickets`."""

    def test_board_renders_seeded_ticket(self, tui, token, bearer):
        marker = f"E2E-{uuid.uuid4().hex[:6]}"
        create_ticket(bearer, marker)

        child = tui("tickets", token=token)
        _expect(child, "Open (1)", marker)
        child.send("q")
        _expect_eof(child)

    def test_board_move_card_updates_api(self, tui, token, bearer):
        """Press L in the TUI to shift a card open -> in_progress, then confirm
        over the API that the move was persisted."""
        marker = f"E2E-{uuid.uuid4().hex[:6]}"
        ticket = create_ticket(bearer, marker)
        tid = ticket["ticket_id"]

        child = tui("tickets", token=token)
        _expect(child, "Open (1)", marker)   # focused card sits in the Open column
        child.send("L")                       # shift-right: move card to In Progress
        _expect(child, "In Progress (1)")     # board refetches and re-renders
        child.send("q")
        _expect_eof(child)

        res = requests.get(f"{API_URL}/tickets/tickets/{tid}", headers=bearer)
        assert res.status_code == 200
        assert res.json()["status"] == "in_progress", \
            "card move in the TUI did not persist to the backend"

        requests.delete(f"{API_URL}/tickets/tickets/{tid}", headers=bearer)

    def test_board_unauthenticated_shows_error_screen(self, tui):
        """With no token the board's data fetch 401s; the TUI must render its
        auth-error guidance rather than crashing or hanging."""
        child = tui("tickets")  # no token
        _expect(child, "Error", "Not authenticated")
        child.send("q")
        _expect_eof(child)


# ---------------------------------------------------------------------------
# CI TUI — `armory ci tui`
# ---------------------------------------------------------------------------

class TestCITUI:
    """The pipelines/runs browser, driven via `armory ci tui`."""

    def test_ci_renders_pipeline_list(self, tui, token, bearer):
        name = f"e2e-pl-{uuid.uuid4().hex[:6]}"
        step = create_healthz_step(bearer)
        pipeline = create_pipeline(bearer, [step["step_id"]], name)
        wf_id = pipeline["workflow_id"]

        child = tui("ci", "tui", token=token)
        _expect(child, "Pipelines", name)
        child.send("q")
        _expect_eof(child)

        requests.delete(f"{API_URL}/workflows/pipelines/{wf_id}", headers=bearer)
        requests.delete(f"{API_URL}/workflows/steps/{step['step_id']}", headers=bearer)

    def test_ci_enter_drills_into_runs(self, tui, token, bearer):
        """Enter on a pipeline navigates from the Pipelines list to its Runs
        view, whose title embeds the selected pipeline name."""
        name = f"e2e-pl-{uuid.uuid4().hex[:6]}"
        step = create_healthz_step(bearer)
        pipeline = create_pipeline(bearer, [step["step_id"]], name)
        wf_id = pipeline["workflow_id"]
        run = trigger_run(bearer, wf_id)
        run_id = run["run_id"]

        child = tui("ci", "tui", token=token)
        _expect(child, "Pipelines", name)
        child.send("\r")                 # enter: open the selected pipeline's runs
        _expect(child, f"Runs: {name}")  # runs view title carries the pipeline name
        child.send("q")
        _expect_eof(child)

        requests.delete(f"{API_URL}/workflows/runs/{run_id}", headers=bearer)
        requests.delete(f"{API_URL}/workflows/pipelines/{wf_id}", headers=bearer)
        requests.delete(f"{API_URL}/workflows/steps/{step['step_id']}", headers=bearer)


# ---------------------------------------------------------------------------
# CI TUI — command registration (cheap, binary-only; no stack required)
# ---------------------------------------------------------------------------

class TestCITUICommand:
    """`armory ci tui --help` must be registered and coherent."""

    def test_ci_tui_help_exits_zero(self, run_cli):
        _, _, rc = run_cli("ci", "tui", "--help")
        assert rc == 0

    def test_ci_tui_help_describes_interactive_mode(self, run_cli):
        out, _, _ = run_cli("ci", "tui", "--help")
        assert any(kw in out.lower() for kw in ("browse", "interactive", "pipeline"))

    def test_ci_tui_rejects_unexpected_args(self, run_cli):
        _, _, rc = run_cli("ci", "tui", "unexpected-arg")
        assert rc != 0
