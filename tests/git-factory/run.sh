#!/usr/bin/env bash
# Manage the codearmory_git_factory integration-test stack and run the tests.
#
# The dependencies (postgres + registry + gatekeeper) run in docker-compose; the
# service under test runs on the HOST via `go run .`, connecting to the
# compose-published postgres (localhost:5432) and gatekeeper (localhost:8081).
# Tests run in an isolated venv (the global pytest env may carry clashing plugins).
#
# Usage:
#   ./run.sh up             # start deps (compose) + the service (go run .)
#   ./run.sh down           # stop the service + deps (wipes volumes)
#   ./run.sh logs           # tail the service log
#   ./run.sh [pytest args]  # run tests (e.g. ./run.sh -v)
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(pwd)"

# ── config ──────────────────────────────────────────────────────────────────
SERVICE_DIR="../../src/systems/git-factory"                  # dir holding go.mod + main
PORT="9002"
CODEARMORY_GIT_FACTORY_URL="${CODEARMORY_GIT_FACTORY_URL:-http://localhost:${PORT}}"
GATEKEEPER_URL="${GATEKEEPER_URL:-http://localhost:8081}"
DATABASE_URL="${DATABASE_URL:-postgres://postgres:postgres@localhost:5432/codearmory_git_factory}"
GATEKEEPER_SERVICE_KEY="${GATEKEEPER_SERVICE_KEY:-codearmory_git_factory-local-secret}"

RUNDIR=".run"
PIDFILE="${RUNDIR}/service.pid"
LOGFILE="${RUNDIR}/service.log"

service_running() { [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }

health_ok() {
  python3 - "${CODEARMORY_GIT_FACTORY_URL}/healthz" <<'PY' 2>/dev/null
import sys, urllib.request
try:
    urllib.request.urlopen(sys.argv[1], timeout=2)
except Exception:
    sys.exit(1)
PY
}

start_service() {
  mkdir -p "$RUNDIR"
  if service_running; then echo "service already running (pid $(cat "$PIDFILE"))"; return 0; fi
  echo "→ starting service: (cd ${SERVICE_DIR} && go run .) on :${PORT}"
  export DATABASE_URL GATEKEEPER_URL GATEKEEPER_SERVICE_KEY PORT
  # setsid → the service gets its own process group so `down` can kill go run and
  # the binary it spawns together. $$ inside is that group's leader (== PGID).
  setsid bash -c 'echo $$ > "$1"; cd "$2"; exec go run .' _ "${ROOT}/${PIDFILE}" "${SERVICE_DIR}" \
    >"$LOGFILE" 2>&1 &
  for _ in $(seq 1 90); do
    if health_ok; then echo "✓ service healthy at ${CODEARMORY_GIT_FACTORY_URL}"; return 0; fi
    if ! service_running; then echo "✗ service exited early — last log lines:"; tail -n 30 "$LOGFILE"; return 1; fi
    sleep 1
  done
  echo "✗ service not healthy after 90s — last log lines:"; tail -n 30 "$LOGFILE"; return 1
}

stop_service() {
  if [ -f "$PIDFILE" ]; then
    pid="$(cat "$PIDFILE")"
    kill -TERM "-${pid}" 2>/dev/null || kill -TERM "${pid}" 2>/dev/null || true
    rm -f "$PIDFILE"
    echo "→ stopped service (pgid ${pid})"
  fi
}

case "${1:-}" in
  up)
    docker compose up -d --build --wait     # postgres + registry + gatekeeper
    start_service
    exit 0 ;;
  down)
    stop_service
    docker compose down -v
    exit 0 ;;
  logs)
    exec tail -n +1 -f "$LOGFILE" ;;
esac

if [ ! -d .venv ]; then
  python3 -m venv .venv
  .venv/bin/pip install -q -r requirements.txt
fi

export CODEARMORY_GIT_FACTORY_URL GATEKEEPER_URL
exec .venv/bin/python -m pytest "$@"
