#!/usr/bin/env bash
# Milestone 7: the full failover scenario with the real cove binary against the sandbox
# organisation on real GitHub (the demo's steps, design section 11). Uses the cove-rw App from
# .secrets/. Every branch, issue and PR it makes is named sb-<run>, so reruns don't collide.
# GitHub is never blocked: "down" is the admin's `cove failover start`, and GitHub-side changes
# go through the App's API meanwhile. Exits non-zero if any check fails.
. "$(dirname "$0")/lib.sh" sandbox
set -e
RUN=sb-$(date -u +%m%d%H%M%S)
ROOT=$(cd "$HERE/.." && pwd)
D=$WORK/$RUN
mkdir -p "$D"
PORT=18473
COVE=$D/cove
(cd "$ROOT" && go build -o "$COVE" ./cmd/cove)
export COVE_ORG=$ORG COVE_APP_ID=$RW_ID COVE_APP_KEY_FILE=$S/cove-rw.pem COVE_DATA=$D/data \
  COVE_LISTEN=127.0.0.1:$PORT COVE_SYNC_INTERVAL=1m COVE_REPLAY_INTERVAL=5s GIT_TERMINAL_PROMPT=0
U=http://127.0.0.1:$PORT
TOKEN=$(mint rw)
fails=0
check(){ if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fails=$((fails+1)); fi; }
now(){ date +%s; }
serve(){ "$COVE" serve >>"$D/serve.log" 2>&1 & SRV=$!; until curl -fsS $U/status >/dev/null 2>&1; do sleep 0.2; done; }
trap 'kill $SRV 2>/dev/null' EXIT
# web TOKEN PATH [curl form args]: a form post as that person; prints "status location"
web(){ local t=$1 p=$2; shift 2; curl -sS -o "$D/web.out" -w '%{http_code} %{redirect_url}\n' -u "x:$t" "$@" "$U/$R$p"; }
mode(){ curl -fsS $U/status | jq -r .mode; }

echo "## $RUN: GitHub before the outage"
MAIN=$(api "$B/git/ref/heads/main" | jq -r .object.sha)
api -X POST "$B/git/refs" -d "{\"ref\":\"refs/heads/$RUN-diverge\",\"sha\":\"$MAIN\"}" | jq -r .ref
BASE=$(api -X POST "$B/issues" -d "{\"title\":\"$RUN base\",\"body\":\"before the outage\"}" | jq -r .number)
echo "issue #$BASE"

t0=$(now); "$COVE" sync; echo "first sync (backup) took $(( $(now)-t0 )) s; data is $(du -sh "$D/data" | cut -f1)"
BEN=$("$COVE" token create -name ben -repo "$R")
ALICE=$("$COVE" token create -name alice -repo "$R")
serve
# GitHub's issue listing lags a few seconds behind a create (spike M), so the sync above can miss
# the issue made just before it. The next sync has it.
extra=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' -u "x:$BEN" "$U/$R/issues/$BASE")" = 200 ]; do
  [ $extra -lt 5 ] || { echo "FAIL issue #$BASE never reached the mirror"; exit 1; }
  sleep 3; "$COVE" sync; extra=$((extra+1))
done
echo "issue #$BASE was in the mirror after $extra extra sync(s)"

echo "## failover: developers work on GitCove"
"$COVE" failover start
git clone -q "http://x:$BEN@127.0.0.1:$PORT/$R.git" "$D/dev" && cd "$D/dev"
git config user.name ben; git config user.email ben@example.invalid
git checkout -q -b "$RUN-feature" origin/main
printf 'one\ntwo\n' > "$RUN.txt"; git add .; git commit -qm "$RUN feature"; FEATURE=$(git rev-parse HEAD)
git push -q origin "$RUN-feature" 2>&1 | grep GitCove:
git checkout -q -b "$RUN-diverge" "origin/$RUN-diverge"
echo cove > "$RUN-cove.txt"; git add .; git commit -qm "$RUN on GitCove"; DIVERGE=$(git rev-parse HEAD)
git push -q origin "$RUN-diverge" 2>&1 | grep GitCove:
git checkout -q -B main origin/main; git commit -q --allow-empty -m "$RUN straight to main"
check "push to protected main is refused" "$(git push origin main 2>&1 | grep -c 'protected')" 1
cd "$HERE"

check "comment" "$(web "$BEN" "/issues/$BASE/comment" --data-urlencode "body=$RUN comment from the outage" | cut -d' ' -f1)" 303
check "title edit" "$(web "$BEN" "/issues/$BASE/edit" --data-urlencode "title=$RUN base, edited on GitCove" --data-urlencode "body=before the outage" | cut -d' ' -f1)" 303
check "new issue" "$(web "$BEN" /issues --data-urlencode "title=$RUN new issue" --data-urlencode "body=opened during the outage" | cut -d' ' -f1)" 303
NEW=$(sed -n 's/.*\(L-[0-9]*\).*/\1/p' "$D/web.out" | head -1); [ -n "$NEW" ] || NEW=L-1
loc=$(web "$BEN" /pulls --data-urlencode "head=$RUN-feature" --data-urlencode "title=$RUN PR" --data-urlencode "body=Fixes #L-1")
check "open PR" "${loc%% *}" 303
PR=${loc##*/issues/}
check "review with a line comment, approving" "$(web "$ALICE" "/issues/$PR/review" -d verdict=approve -d "commit=$FEATURE" \
  --data-urlencode "body=$RUN review" --data-urlencode "c|RIGHT|2|$RUN.txt=$RUN line comment" | cut -d' ' -f1)" 303
check "local merge" "$(web "$ALICE" "/issues/$PR/merge" -X POST | cut -d' ' -f1)" 303

echo "## meanwhile on GitHub: a commit on the same branch, and the same title edited"
api -X PUT "$B/contents/$RUN-github.txt" -d "{\"message\":\"$RUN on GitHub\",\"content\":\"Z2l0aHViCg==\",\"branch\":\"$RUN-diverge\"}" | jq -r .commit.sha > "$D/gh-diverge"
api -X PATCH "$B/issues/$BASE" -d "{\"title\":\"$RUN base, edited on GitHub\"}" | jq -r .title

echo "## GitHub is back: replay, with GitCove killed partway"
"$COVE" failover end | sed 's/^/    /'
queued=$(curl -fsS $U/status | jq .unreplayed)
t0=$(now); "$COVE" replay approve
until [ "$(curl -fsS $U/status | jq .unreplayed)" -lt "$queued" ]; do sleep 0.2; done
kill -9 $SRV; wait $SRV 2>/dev/null || true
echo "killed GitCove with $(sqlite3 "$D/data/cove.db" "select count(*) from outbox where state='sending'") item(s) mid-send"
serve
for _ in $(seq 120); do [ "$(mode)" = normal ] && break; sleep 2; done
echo "replay of $queued changes took $(( $(now)-t0 )) s, including the kill and restart"
"$COVE" replay plan | sed 's/^/    /'
"$COVE" sync

echo "## checks against GitHub"
TOKEN=$(mint rw)
count(){ api "$1" | jq "[.[] | select($2)] | length"; }
check "mode" "$(mode)" normal
check "main didn't move" "$(api "$B/git/ref/heads/main" | jq -r .object.sha)" "$MAIN"
check "feature branch has the developer's commit" "$(api "$B/git/ref/heads/$RUN-feature" | jq -r .object.sha)" "$FEATURE"
check "diverged branch still has GitHub's commit" "$(api "$B/git/ref/heads/$RUN-diverge" | jq -r .object.sha)" "$(cat "$D/gh-diverge")"
check "the outage commit is on one fallback branch" "$(api "$B/commits/$DIVERGE/branches-where-head" | jq -r 'map(.name)|join(",")' | grep -c "^gitcove/$RUN-diverge-[^,]*$")" 1
check "one PR for the fallback branch" "$(count "$B/pulls?state=all&base=$RUN-diverge" true)" 1
check "one PR for the feature" "$(count "$B/pulls?state=all&head=$ORG:$RUN-feature" true)" 1
N=$(api "$B/pulls?state=all&head=$ORG:$RUN-feature" | jq '.[0].number')
check "comment posted once" "$(count "$B/issues/$BASE/comments?per_page=100" ".body|contains(\"$RUN comment from the outage\")")" 1
check "GitHub's title kept" "$(api "$B/issues/$BASE" | jq -r .title)" "$RUN base, edited on GitHub"
check "the other title proposed in a comment" "$(count "$B/issues/$BASE/comments?per_page=100" ".body|contains(\"edited on GitCove\")")" 1
check "new issue posted once" "$(count "$B/issues?state=all&per_page=100&sort=created" ".title==\"$RUN new issue\"")" 1
NEWN=$(api "$B/issues?state=all&per_page=100&sort=created" | jq "[.[]|select(.title==\"$RUN new issue\")][0].number")
check "PR body names the new issue's real number" "$(api "$B/pulls/$N" | jq -r .body | grep -c "Fixes #$NEWN")" 1
check "one review" "$(count "$B/pulls/$N/reviews" ".body|contains(\"$RUN review\")")" 1
check "one line comment, on the reviewed commit" "$(count "$B/pulls/$N/comments" "(.body|contains(\"$RUN line comment\")) and .original_commit_id==\"$FEATURE\"")" 1
check "no approval counts: the reviewer never linked an account" "$(count "$B/pulls/$N/reviews" '.state=="APPROVED"')" 0
check "one note that it was merged on GitCove" "$(count "$B/issues/$N/comments" '.body|contains("on GitCove at commit")')" 1
check "PR still open for a person to merge" "$(api "$B/pulls/$N" | jq -r .state)" open
gh=$(git ls-remote "https://x-access-token:$TOKEN@github.com/$R.git" 'refs/heads/*' 'refs/tags/*' | sort)
cove=$(git ls-remote "http://x:$BEN@127.0.0.1:$PORT/$R.git" 'refs/heads/*' 'refs/tags/*' | sort)
check "after a sync GitCove serves exactly GitHub's branches and tags" "$cove" "$gh"
echo "## $RUN: $fails failed"
exit $fails
