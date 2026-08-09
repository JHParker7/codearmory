#!/usr/bin/env bash
# The cross-pod deployment check.
#
# storagecheck.go is explicit that it verifies its three properties LOCALLY, in one
# process on one node, and that "verifying the cross-client half needs two pods against
# one mount and belongs in the deployment checklist, not here". This is that checklist,
# executed: two git_factory replicas, one JuiceFS mount, the properties git actually
# depends on.
#
# Run after setup.sh. Read-only apart from scratch files it removes on the way out.
set -uo pipefail

NS=${NS_APP:-gitfactory}
fails=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fails=$((fails+1)); }
say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }

A=$(kubectl get pod -n "$NS" -l app=git-factory -o jsonpath='{.items[0].metadata.name}')
B=$(kubectl get pod -n "$NS" -l app=git-factory -o jsonpath='{.items[1].metadata.name}')
if [[ -z "$A" || -z "$B" || "$A" == "$B" ]]; then
  echo "need two git-factory pods; found '$A' and '$B'" >&2
  exit 1
fi
echo "pod A: $A"
echo "pod B: $B"
inA() { kubectl exec -n "$NS" "$A" -c git-factory -- sh -c "$1" 2>&1; }
inB() { kubectl exec -n "$NS" "$B" -c git-factory -- sh -c "$1" 2>&1; }

say "0. Both replicas passed the startup preflight"
for p in "$A" "$B"; do
  # Capture first rather than piping into `grep -q`: grep exits on the first match, which
  # SIGPIPEs kubectl, and under `pipefail` that turns a passing check into a failing one
  # once the log is long enough for the race to land.
  plog=$(kubectl logs -n "$NS" "$p" -c git-factory 2>/dev/null)
  if printf '%s' "$plog" | grep -q 'storage preflight: passed'; then
    ok "$p preflight passed"
  else
    bad "$p did not log a passing preflight"
  fi
  r=$(kubectl get pod -n "$NS" "$p" -o jsonpath='{.status.containerStatuses[0].restartCount}')
  # Replicas start together and probe the same directory. Restarts here are the
  # signature of preflight probes colliding (fixed since probe names carry a per-pod id).
  [[ "$r" == "0" ]] && ok "$p has not restarted" || bad "$p restarted $r times — check for preflight collisions"
done

say "1. The two pods see one filesystem"
a=$(inA 'find /data/repos -maxdepth 2 -name "*.git" | wc -l' | tail -1)
b=$(inB 'find /data/repos -maxdepth 2 -name "*.git" | wc -l' | tail -1)
[[ "$a" == "$b" ]] && ok "both see $a repos" || bad "A sees $a repos, B sees $b"
inA 'printf hello > /data/repos/.xpod-visible' >/dev/null
seen=$(inB 'cat /data/repos/.xpod-visible' | tail -1)
[[ "$seen" == "hello" ]] && ok "a write in A is visible in B" || bad "B read '$seen', want 'hello'"
inA 'rm -f /data/repos/.xpod-visible' >/dev/null

say "2. O_CREAT|O_EXCL is exclusive ACROSS pods (git's ref-lock primitive)"
inA 'rm -f /data/repos/.xpod-excl' >/dev/null
first=$(inA '( set -C; printf "" > /data/repos/.xpod-excl ) && echo created || echo refused' | tail -1)
second=$(inB '( set -C; printf "" > /data/repos/.xpod-excl ) 2>/dev/null && echo created || echo refused' | tail -1)
[[ "$first" == "created" ]] && ok "A created the lock" || bad "A could not create the lock"
# The one that matters: if B is also granted, two pods hold the same ref lock and a
# concurrent push silently loses an update.
[[ "$second" == "refused" ]] && ok "B was refused while it exists" || bad "B ALSO created it — ref locks do not serialise across pods"
inA 'rm -f /data/repos/.xpod-excl' >/dev/null
after=$(inB '( set -C; printf "" > /data/repos/.xpod-excl ) 2>/dev/null && echo created || echo refused' | tail -1)
[[ "$after" == "created" ]] && ok "B may create it once A removes it" || bad "B still refused after removal — the lock is stuck"
inB 'rm -f /data/repos/.xpod-excl' >/dev/null

say "3. flock is ENFORCED across pods, not merely accepted"
inA 'rm -f /data/repos/.xpod-lock; touch /data/repos/.xpod-lock' >/dev/null
control=$(inB 'exec 9>/data/repos/.xpod-lock; flock -n 9 && echo granted || echo refused' | tail -1)
[[ "$control" == "granted" ]] && ok "control: an uncontended lock is granted" || bad "control: an uncontended lock was refused"
# Hold the lock in A for a while, and ask B for it in the middle. Note flock -c runs
# the command through the account's shell, which is nologin for this image's user —
# hence the explicit fd form.
inA 'exec 9>/data/repos/.xpod-lock; flock -n 9 || exit 9; sleep 12' >/dev/null &
holder=$!
sleep 4
during=$(inB 'exec 9>/data/repos/.xpod-lock; flock -n 9 && echo granted || echo refused' | tail -1)
[[ "$during" == "refused" ]] && ok "B is refused while A holds it" || bad "B was GRANTED while A held it — locking is accepted but not enforced"
wait $holder 2>/dev/null
sleep 2
after=$(inB 'exec 9>/data/repos/.xpod-lock; flock -n 9 && echo granted || echo refused' | tail -1)
[[ "$after" == "granted" ]] && ok "B gets it once A releases" || bad "B still refused after release — the lock leaked"
inA 'rm -f /data/repos/.xpod-lock' >/dev/null

say "4. A real concurrent push to one branch from both pods"
GC='git -c user.email=t@t -c user.name=t'
inA "rm -rf /data/repos/.xpod-race.git /tmp/seed
     git init --bare -q /data/repos/.xpod-race.git
     git --git-dir=/data/repos/.xpod-race.git symbolic-ref HEAD refs/heads/main
     mkdir -p /tmp/seed && cd /tmp/seed && git init -q .
     echo base > f.txt && $GC add f.txt && $GC commit -qm base
     $GC push -q /data/repos/.xpod-race.git HEAD:refs/heads/main" >/dev/null
push() { # $1 = in-fn, $2 = label
  $1 "rm -rf /tmp/race$2 && git clone -q /data/repos/.xpod-race.git /tmp/race$2 && cd /tmp/race$2
      git checkout -q -B main origin/main
      echo $2 > $2.txt && $GC add $2.txt && $GC commit -qm 'from $2'
      $GC push -q origin main >/dev/null 2>&1 && echo ACCEPTED || echo REJECTED" | tail -1
}
# Both pushes must be in flight at once, so run them in subshells writing to files —
# a command substitution would serialise them and test nothing.
tmp=$(mktemp -d)
( push inA A > "$tmp/a" ) &
( push inB B > "$tmp/b" ) &
wait
ra=$(cat "$tmp/a"); rb=$(cat "$tmp/b"); rm -rf "$tmp"
accepted=0
[[ "$ra" == "ACCEPTED" ]] && accepted=$((accepted+1))
[[ "$rb" == "ACCEPTED" ]] && accepted=$((accepted+1))
# Exactly one winner is the whole claim of §5 Step 3: one copy of each repo means
# nothing can diverge, and git's own locking resolves the race as it would on one host.
[[ "$accepted" == "1" ]] && ok "exactly one push won (A=$ra B=$rb)" || bad "$accepted pushes accepted (A=$ra B=$rb) — want exactly 1"
refA=$(inA 'git --git-dir=/data/repos/.xpod-race.git rev-parse main' | tail -1)
refB=$(inB 'git --git-dir=/data/repos/.xpod-race.git rev-parse main' | tail -1)
[[ "$refA" == "$refB" && -n "$refA" ]] && ok "both pods read the same ref ($refA)" || bad "refs differ: A=$refA B=$refB"
fsckout=$(inA 'git --git-dir=/data/repos/.xpod-race.git fsck --no-progress')
if printf '%s' "$fsckout" | grep -qiE '^(error|fatal|missing|broken)'; then
  bad "fsck reported damage after the race"
else
  ok "fsck reports no damage (dangling objects from the losing push are expected)"
fi

# Remove the scratch repo before counting, so section 5 reports the real store.
inA 'rm -rf /data/repos/.xpod-race.git /tmp/raceA /tmp/seed' >/dev/null
inB 'rm -rf /tmp/raceB' >/dev/null

say "5. Every repo in the store is intact"
res=$(inA 'ok=0; bad=0
  for r in $(find /data/repos -maxdepth 2 -name "*.git"); do
    if git --git-dir="$r" fsck --no-progress >/dev/null 2>&1; then ok=$((ok+1)); else bad=$((bad+1)); fi
  done; echo "$ok $bad"' | tail -1)
set -- $res
[[ "${2:-1}" == "0" ]] && ok "$1 repos fsck clean, 0 damaged" || bad "$2 repos failed fsck"

say "6. The store is swept once per interval, not once per replica"
# The other half of running N replicas against one store. Sections 1-5 cover whether
# concurrent access is SAFE; this covers whether it is affordable. Every replica ticks
# the maintenance loop, so without the lease in git-factory/lease.go each one walks the
# whole store every interval — N times the repack cost for the same work, and it gets
# worse exactly as the HPA scales out.
#
# Note this cannot be checked by watching for overlap: replicas tick on their own
# offsets, so the failure is usually sweeps that are SEQUENTIAL and redundant rather
# than simultaneous. Counting completions over a window is what distinguishes them.
# The service logs JSON (main.go uses slog.NewJSONHandler), hence the quoted form —
# a logfmt-style `lease_holder=...` match would silently find nothing and report 0.
holders=$(for p in "$A" "$B"; do
  kubectl logs -n "$NS" "$p" -c git-factory 2>/dev/null \
    | grep -o '"lease_holder":"[^"]*"' | head -1
done | sort -u | wc -l)
# Two replicas must present two identities, or renewal and release cross-talk: a pod
# would be able to renew or release a lease another pod holds.
[[ "$holders" == "2" ]] && ok "the two pods use distinct lease holder ids" \
  || bad "the pods reported $holders distinct lease_holder ids — want 2"

interval=$(kubectl get deploy/git-factory -n "$NS" \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="GIT_MAINTENANCE_INTERVAL")].value}')
# Only the two forms this deployment actually uses, matched whole. A compound Go
# duration like "2h30m" must not be half-parsed into a number — anything unrecognised
# falls through to a value that trips the skip below rather than being guessed at.
if [[ "$interval" =~ ^([0-9]+)s$ ]]; then
  isecs=${BASH_REMATCH[1]}
elif [[ "$interval" =~ ^([0-9]+)m$ ]]; then
  isecs=$(( ${BASH_REMATCH[1]} * 60 ))
else
  isecs=99999
fi

sweeps() { # total "sweep complete" lines across both pods
  local n=0
  for p in "$A" "$B"; do
    c=$(kubectl logs -n "$NS" "$p" -c git-factory 2>/dev/null | grep -c 'maintenance: sweep complete' || true)
    n=$(( n + c ))
  done
  echo "$n"
}

if (( isecs > 120 )); then
  # Deliberately not a failure. Against a production-shaped interval this check would
  # take hours; 50-git-factory.yaml sets 30s locally precisely so it can run.
  printf '  \033[33mSKIP\033[0m GIT_MAINTENANCE_INTERVAL=%s is too long to observe (set 30s to check this)\n' "${interval:-unset}"
else
  window=$(( isecs * 3 + 10 ))
  before=$(sweeps)
  echo "  watching for ${window}s (3 x ${isecs}s intervals, 2 replicas)"
  sleep "$window"
  after=$(sweeps)
  did=$(( after - before ))
  # One sweep per interval, plus one for landing mid-interval at either end. Two
  # replicas sweeping unguarded would produce roughly double this.
  if (( did >= 1 && did <= 4 )); then
    ok "$did sweeps across both pods in 3 intervals (one replica per interval)"
  elif (( did < 1 )); then
    bad "no sweep completed in ${window}s — maintenance is not running at all"
  else
    bad "$did sweeps across both pods in 3 intervals — want <=4; every replica is sweeping the whole store"
  fi
fi

say "Result"
if [[ "$fails" == "0" ]]; then
  echo "All cross-pod checks passed."
else
  echo "$fails check(s) FAILED — do not run more than one replica against this mount."
  exit 1
fi
