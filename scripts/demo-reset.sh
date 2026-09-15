#!/usr/bin/env bash
# Demo reset for the LOCAL codearmory minikube cluster.
# Logs in with your gatekeeper email + password, then deletes ALL:
#   - tickets (boards are KEPT)
#   - git-factory repos
#   - workflow runs
# KEEPS: blacksmith roles (agents) and workflow pipelines (never touched).
#
# Usage:
#   ./demo-reset.sh                       # prompts for email + password
#   ./demo-reset.sh you@example.com       # prompts for password only
#   CA_EMAIL=you@x CA_PASSWORD=secret ./demo-reset.sh
#
# Requires: kubectl (pointing at the codearmory cluster), python3, curl.
set -uo pipefail
NS=codearmory

EMAIL="${1:-${CA_EMAIL:-}}"
PASSWORD="${CA_PASSWORD:-}"
[ -n "$EMAIL" ]    || { read -r -p "gatekeeper email: " EMAIL; }
[ -n "$PASSWORD" ] || { read -r -s -p "gatekeeper password: " PASSWORD; echo; }
[ -n "$EMAIL" ] && [ -n "$PASSWORD" ] || { echo "ERROR: email and password required"; exit 2; }

cleanup(){ for p in ${PF_PIDS:-}; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

echo "== port-forwarding services =="
kubectl -n $NS port-forward svc/codearmory-gatekeeper  18081:8081 >/dev/null 2>&1 & PF_PIDS="$!"
kubectl -n $NS port-forward svc/codearmory-tickets     18086:8086 >/dev/null 2>&1 & PF_PIDS="$PF_PIDS $!"
kubectl -n $NS port-forward svc/codearmory-git-factory 19002:9002 >/dev/null 2>&1 & PF_PIDS="$PF_PIDS $!"
kubectl -n $NS port-forward svc/codearmory-workflows   18085:8085 >/dev/null 2>&1 & PF_PIDS="$PF_PIDS $!"
sleep 3

GK=http://127.0.0.1:18081
TICKETS=http://127.0.0.1:18086
GITF=http://127.0.0.1:19002
WF=http://127.0.0.1:18085

echo "== logging in as $EMAIL =="
LOGIN=$(curl -s -X POST "$GK/login" -H 'Content-Type: application/json' \
  --data-binary "$(python3 -c 'import json,sys; print(json.dumps({"email":sys.argv[1],"password":sys.argv[2]}))' "$EMAIL" "$PASSWORD")")
TOK=$(printf '%s' "$LOGIN" | python3 -c 'import sys,json
try: d=json.load(sys.stdin)
except: sys.exit(0)
if d.get("mfa_required"): sys.stderr.write("MFA is enabled on this account — this script does not handle TOTP. Use a token from the portal instead.\n"); sys.exit(0)
print(d.get("token",""))')
[ -n "$TOK" ] || { echo "ERROR: login failed (bad credentials, MFA, or wrong cluster). Response:"; printf '%s\n' "$LOGIN" | head -c 300; echo; exit 3; }
AUTH=(-H "Authorization: Bearer $TOK")
echo "   ok"

ids_from() { python3 -c 'import sys,json
try: d=json.load(sys.stdin)
except: sys.exit(0)
rows = d if isinstance(d,list) else (d.get("items") or d.get("tickets") or d.get("repos") or d.get("runs") or [])
for r in rows:
    v = r.get(sys.argv[1]) or r.get("id")
    if v: print(v)' "$1" 2>/dev/null; }

echo "== TICKETS (delete all, keep boards) =="
n=0; for id in $(curl -s "${AUTH[@]}" "$TICKETS/tickets" | ids_from ticket_id); do
  code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "${AUTH[@]}" "$TICKETS/tickets/$id")
  echo "  ticket $id -> $code"; n=$((n+1)); done
echo "  tickets deleted: $n"

echo "== REPOS (delete all) =="
n=0; for id in $(curl -s "${AUTH[@]}" "$GITF/repos" | ids_from id); do
  code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "${AUTH[@]}" "$GITF/repos/$id")
  echo "  repo $id -> $code"; n=$((n+1)); done
echo "  repos deleted: $n"

echo "== RUNS (delete all) =="
n=0; for id in $(curl -s "${AUTH[@]}" "$WF/runs" | ids_from id); do
  code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "${AUTH[@]}" "$WF/runs/$id")
  echo "  run $id -> $code"; n=$((n+1)); done
echo "  runs deleted: $n"

echo "== KEPT (verify) =="
echo -n "  pipelines: "; curl -s "${AUTH[@]}" "$WF/pipelines"    | python3 -c 'import sys,json;d=json.load(sys.stdin);print(len(d if isinstance(d,list) else d.get("items",[])))' 2>/dev/null || echo '?'
echo -n "  boards:    "; curl -s "${AUTH[@]}" "$TICKETS/boards"  | python3 -c 'import sys,json;d=json.load(sys.stdin);print(len(d if isinstance(d,list) else d.get("items",[])))' 2>/dev/null || echo '?'
echo "done — agents (roles) and workflows (pipelines) untouched."
