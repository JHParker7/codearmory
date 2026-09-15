#!/usr/bin/env bash
# Run the rest of the agent flow after arch+pm: create+seed a target repo, then
# trigger the backend and frontend dev pipelines against the board tickets.
# (pr-review + devops follow once the dev PRs land — see the printed follow-up.)
# You supply gatekeeper creds; this is the LOCAL dev cluster.
#   ./run-rest.sh                     # prompts for email + password
#   CA_EMAIL=you@x CA_PASSWORD=... ./run-rest.sh
set -uo pipefail
NS_K8S=codearmory
PROJECT="${CA_PROJECT:-ops}"
REPO="${CA_REPO:-task-tracker}"
BASE_REF="${CA_REF:-dev}"

EMAIL="${1:-${CA_EMAIL:-}}"; PASSWORD="${CA_PASSWORD:-}"
[ -n "$EMAIL" ]    || read -r -p "gatekeeper email: " EMAIL
[ -n "$PASSWORD" ] || { read -r -s -p "gatekeeper password: " PASSWORD; echo; }

cleanup(){ for p in ${PF:-}; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT
kubectl -n $NS_K8S port-forward svc/codearmory-gatekeeper  18081:8081 >/dev/null 2>&1 & PF="$!"
kubectl -n $NS_K8S port-forward svc/codearmory-git-factory 19002:9002 >/dev/null 2>&1 & PF="$PF $!"
kubectl -n $NS_K8S port-forward svc/codearmory-workflows   18085:8085 >/dev/null 2>&1 & PF="$PF $!"
sleep 3
GK=http://127.0.0.1:18081; G=http://127.0.0.1:19002; W=http://127.0.0.1:18085

echo "== login =="
TOK=$(curl -s -X POST "$GK/login" -H 'Content-Type: application/json' \
  --data-binary "$(python3 -c 'import json,sys;print(json.dumps({"email":sys.argv[1],"password":sys.argv[2]}))' "$EMAIL" "$PASSWORD")" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin).get("token",""))' 2>/dev/null)
[ -n "$TOK" ] || { echo "login failed"; exit 3; }
A=(-H "Authorization: Bearer $TOK")

echo "== resolve pipeline ids =="
PIPES=$(curl -s "${A[@]}" "$W/pipelines")
pid_of(){ printf '%s' "$PIPES" | python3 -c 'import sys,json
rows=json.load(sys.stdin); rows=rows if isinstance(rows,list) else rows.get("items",[])
for p in rows:
  if p.get("name")==sys.argv[1]: print(p.get("workflow_id") or p.get("id") or ""); break' "$1"; }
BACKEND=$(pid_of backend); FRONTEND=$(pid_of frontend)
echo "   backend=$BACKEND frontend=$FRONTEND"
[ -n "$BACKEND" ] && [ -n "$FRONTEND" ] || { echo "could not resolve backend/frontend pipeline ids"; exit 4; }

echo "== create repo '$REPO' =="
CR=$(curl -s -X POST "${A[@]}" -H 'Content-Type: application/json' "$G/repos" \
  --data-binary "$(python3 -c 'import json,sys;print(json.dumps({"name":sys.argv[1],"visibility":"private"}))' "$REPO")")
NSPACE=$(printf '%s' "$CR" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("namespace",""))' 2>/dev/null)
if [ -z "$NSPACE" ]; then   # already exists? find it
  NSPACE=$(curl -s "${A[@]}" "$G/repos" | python3 -c 'import sys,json
rows=json.load(sys.stdin); rows=rows if isinstance(rows,list) else rows.get("repos",rows.get("items",[]))
for r in rows:
  if r.get("name")==sys.argv[1]: print(r.get("namespace","")); break' "$REPO")
fi
[ -n "$NSPACE" ] || { echo "could not create/find repo. response:"; printf '%s\n' "$CR" | head -c 300; exit 5; }
echo "   repo=$NSPACE/$REPO"

echo "== seed repo (main + $BASE_REF) =="
TMP=$(mktemp -d); ( cd "$TMP"
  git init -q; git checkout -q -b main
  printf '# %s\n\nScaffolded by the agent demo. Agents open PRs against `%s`.\n' "$REPO" "$BASE_REF" > README.md
  git add -A; git -c user.email=demo@local -c user.name=demo commit -qm "chore: initial commit"
  git remote add origin "http://x:${TOK}@127.0.0.1:19002/${NSPACE}/${REPO}.git"
  git push -q origin main && echo "   pushed main" || echo "   WARN: push main failed"
  git checkout -q -b "$BASE_REF"; git push -q origin "$BASE_REF" && echo "   pushed $BASE_REF" || echo "   WARN: push $BASE_REF failed"
); rm -rf "$TMP"

trigger(){ # $1=pipeline_id  $2=task
  curl -s -X POST "${A[@]}" -H 'Content-Type: application/json' "$W/pipelines/$1/runs" \
    --data-binary "$(python3 -c 'import json,sys
print(json.dumps({"inputs":{"task":sys.argv[1],"project":sys.argv[2],"namespace":sys.argv[3],"repo":sys.argv[4],"ref":sys.argv[5]}}))' \
      "$2" "$PROJECT" "$NSPACE" "$REPO" "$BASE_REF")" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print("   run:",d.get("run_id") or d.get("id") or d)'
}
echo "== trigger backend =="
trigger "$BACKEND" "Implement the backend for $REPO. Read the board tickets tagged [backend] and the project wiki; open a PR to $BASE_REF."
echo "== trigger frontend =="
trigger "$FRONTEND" "Implement the frontend for $REPO. Read the board tickets tagged [frontend] and the project wiki; open a PR to $BASE_REF."

echo ""
echo "Backend + frontend triggered. Watch them in the portal (Workflows) or Repos -> $NSPACE/$REPO -> Pull requests."
echo "Once each dev PR is open, pr-review runs per PR; devops (Dockerfile) follows. Say the word and I'll wire pr-review+devops to auto-run on the PRs."
