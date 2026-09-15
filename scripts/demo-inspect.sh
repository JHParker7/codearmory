#!/usr/bin/env bash
# Inspect the LOCAL codearmory cluster's live pipelines/tickets/repos so we can
# wire + trigger the rest of the agent flow. Read-only. You supply gatekeeper creds.
#   ./demo-inspect.sh                 # prompts for email + password
#   CA_EMAIL=you@x CA_PASSWORD=... ./demo-inspect.sh
set -uo pipefail
NS=codearmory
EMAIL="${1:-${CA_EMAIL:-}}"; PASSWORD="${CA_PASSWORD:-}"
[ -n "$EMAIL" ]    || read -r -p "gatekeeper email: " EMAIL
[ -n "$PASSWORD" ] || { read -r -s -p "gatekeeper password: " PASSWORD; echo; }

cleanup(){ for p in ${PF:-}; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT
kubectl -n $NS port-forward svc/codearmory-gatekeeper  18081:8081 >/dev/null 2>&1 & PF="$!"
kubectl -n $NS port-forward svc/codearmory-tickets     18086:8086 >/dev/null 2>&1 & PF="$PF $!"
kubectl -n $NS port-forward svc/codearmory-git-factory 19002:9002 >/dev/null 2>&1 & PF="$PF $!"
kubectl -n $NS port-forward svc/codearmory-workflows   18085:8085 >/dev/null 2>&1 & PF="$PF $!"
sleep 3
GK=http://127.0.0.1:18081; T=http://127.0.0.1:18086; G=http://127.0.0.1:19002; W=http://127.0.0.1:18085

TOK=$(curl -s -X POST "$GK/login" -H 'Content-Type: application/json' \
  --data-binary "$(python3 -c 'import json,sys;print(json.dumps({"email":sys.argv[1],"password":sys.argv[2]}))' "$EMAIL" "$PASSWORD")" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("token",""))' 2>/dev/null)
[ -n "$TOK" ] || { echo "login failed"; exit 3; }
A=(-H "Authorization: Bearer $TOK")

echo "======== PIPELINES (id | name | inputs | #steps) ========"
curl -s "${A[@]}" "$W/pipelines" | python3 -c '
import sys,json
d=json.load(sys.stdin); rows=d if isinstance(d,list) else d.get("items",[])
for p in rows:
    ins=",".join(i.get("name","") for i in (p.get("inputs") or []))
    st=p.get("steps") or []
    names="->".join((s.get("name") or s.get("id") or "?") for s in st)
    print(f'"'"'{p.get("id")}  |  {p.get("name")}  |  inputs=[{ins}]  |  steps={names}'"'"')'

echo ""; echo "======== TICKETS (id | status | board | project | title) ========"
curl -s "${A[@]}" "$T/tickets" | python3 -c '
import sys,json
d=json.load(sys.stdin); rows=d if isinstance(d,list) else d.get("tickets",d.get("items",[]))
for t in rows:
    print(f'"'"'{t.get("ticket_id")}  |  {t.get("status")}  |  {t.get("board_id")}  |  {t.get("project")}  |  {t.get("title")}'"'"')'

echo ""; echo "======== REPOS ========"
curl -s "${A[@]}" "$G/repos" | python3 -c '
import sys,json
d=json.load(sys.stdin); rows=d if isinstance(d,list) else d.get("repos",d.get("items",[]))
print("(none)" if not rows else "")
for r in rows: print(f'"'"'{r.get("id")}  |  {r.get("namespace")}/{r.get("name")}'"'"')'

echo ""; echo "======== BOARDS ========"
curl -s "${A[@]}" "$T/boards" | python3 -c '
import sys,json
d=json.load(sys.stdin); rows=d if isinstance(d,list) else d.get("items",[])
for b in rows: print(f'"'"'{b.get("board_id")}  |  {b.get("name")}'"'"')'
