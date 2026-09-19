#!/usr/bin/env bash
# Grant the assistant local API access: log into the LOCAL codearmory gatekeeper
# and write a bearer token to a file the assistant can read. Your password is
# never written or printed — only the resulting short-lived token is saved.
#
#   ./grant-access.sh                    # prompts for email + password
#   CA_EMAIL=you@x CA_PASSWORD=... ./grant-access.sh
#
# Re-run this when the token expires (default 24h).
set -uo pipefail
NS=codearmory
TOKEN_FILE=/home/jhp1403/.claude/jobs/1df1e5e7/tmp/local_token.txt

EMAIL="${1:-${CA_EMAIL:-}}"; PASSWORD="${CA_PASSWORD:-}"
[ -n "$EMAIL" ]    || read -r -p "gatekeeper email: " EMAIL
[ -n "$PASSWORD" ] || { read -r -s -p "gatekeeper password: " PASSWORD; echo; }
[ -n "$EMAIL" ] && [ -n "$PASSWORD" ] || { echo "email and password required"; exit 2; }

cleanup(){ [ -n "${PF:-}" ] && kill "$PF" 2>/dev/null || true; }
trap cleanup EXIT
kubectl -n $NS port-forward svc/codearmory-gatekeeper 18081:8081 >/dev/null 2>&1 & PF="$!"
sleep 3

RESP=$(curl -s -X POST http://127.0.0.1:18081/login -H 'Content-Type: application/json' \
  --data-binary "$(python3 -c 'import json,sys;print(json.dumps({"email":sys.argv[1],"password":sys.argv[2]}))' "$EMAIL" "$PASSWORD")")
TOK=$(printf '%s' "$RESP" | python3 -c 'import sys,json
try: d=json.load(sys.stdin)
except: sys.exit(0)
if d.get("mfa_required"): sys.stderr.write("MFA enabled — this script cannot do TOTP.\n"); sys.exit(0)
print(d.get("token",""))')

if [ -z "$TOK" ]; then
  echo "login failed. response:"; printf '%s\n' "$RESP" | head -c 300; echo; exit 3
fi

umask 077
printf '%s' "$TOK" > "$TOKEN_FILE"
chmod 600 "$TOKEN_FILE"
EXP=$(printf '%s' "$TOK" | cut -d. -f2 | python3 -c 'import sys,base64,json,time
b=sys.stdin.read().strip(); b+="="*(-len(b)%4)
try: print(time.strftime("%Y-%m-%d %H:%M", time.localtime(json.loads(base64.urlsafe_b64decode(b))["exp"])))
except: print("?")')
echo "OK — local token written to $TOKEN_FILE (expires $EXP)."
echo "The assistant can now call the local API directly."
