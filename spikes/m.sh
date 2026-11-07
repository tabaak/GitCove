#!/usr/bin/env bash
# Spike M: does the hidden marker survive, how fast do filtered listings show a new item, and what do lost responses do?
. "$(dirname "$0")/lib.sh" m
TOKEN=$(mint rw)
uuid(){ uuidgen | tr A-Z a-z; }
t0(){ date -u -v-5M +%FT%TZ 2>/dev/null || date -u -d '-5 min' +%FT%TZ; }
now(){ perl -MTime::HiRes=time -e 'printf "%.2f", time'; }
comment_hits(){ api "$B/issues/$1/comments?since=$(t0)&per_page=100" | jq --arg u "$2" '[.[]|select(.body|contains($u))]|length'; }
issue_hits(){ api -G "$B/issues" --data-urlencode state=all --data-urlencode "creator=$LOGIN" --data-urlencode sort=created \
  --data-urlencode direction=desc --data-urlencode "since=$(t0)" --data-urlencode per_page=100 | jq --arg u "$1" '[.[]|select(.body|contains($u))]|length'; }
# lag NAME CHECK...: seconds until CHECK prints 1, polling every 0.5 s for up to 60 s
lag(){ local n=$1 st; shift; st=$(now); for _ in $(seq 1 120); do [ "$("$@")" = 1 ] && { echo "$n found after $(echo "$(now) - $st" | bc) s"; return; }; sleep 0.5; done; echo "$n NOT FOUND after 60 s"; }

echo "== 1. round trip"
U=$(uuid)
BODY=$(jq -n --arg b "Originally by \`alice\`

hello
<!-- gitcove:op=$U -->" '{title:"marker test",body:$b}')
RES=$(rec create-issue -X POST "$B/issues" -d "$BODY"); N=$(jq .number <<<"$RES"); LOGIN=$(jq -r .user.login <<<"$RES")
echo "issue #$N by $LOGIN"
rec get-issue-full -H 'Accept: application/vnd.github.full+json' "$B/issues/$N" \
  | jq -c '{raw_has:(.body|contains("gitcove:op")),html_has:(.body_html|contains("gitcove:op")),via:.performed_via_github_app.slug}'
UC=$(uuid)
rec create-comment -X POST "$B/issues/$N/comments" -d "{\"body\":\"c\\n<!-- gitcove:op=$UC -->\"}" | jq -c '{id,user:.user.login}'
rec list-comments "$B/issues/$N/comments?since=$(t0)&per_page=100" > /dev/null
rec list-issues-by-creator -G "$B/issues" --data-urlencode state=all --data-urlencode "creator=$LOGIN" --data-urlencode "since=$(t0)" > /dev/null
echo "listing by creator=$LOGIN finds the issue: $(issue_hits "$U")"

echo "== 2. listing lag right after a 201 (10 comments, 5 issues)"
for i in $(seq 1 10); do
  u=$(uuid); api -X POST "$B/issues/$N/comments" -d "{\"body\":\"lag $i\\n<!-- gitcove:op=$u -->\"}" > /dev/null
  lag "comment $i" comment_hits "$N" "$u"
done
for i in $(seq 1 5); do
  u=$(uuid); api -X POST "$B/issues" -d "{\"title\":\"lag $i\",\"body\":\"<!-- gitcove:op=$u -->\"}" > /dev/null
  lag "issue $i" issue_hits "$u"
done

echo "== 3. lost responses: client gives up after T seconds; did the write happen anyway?"
for t in 0.2 0.4 0.6 0.8 1.0 1.5; do
  u=$(uuid); api --max-time "$t" -o /dev/null -X POST "$B/issues/$N/comments" -d "{\"body\":\"lost $t\\n<!-- gitcove:op=$u -->\"}"; rc=$?
  sleep 5; echo "timeout ${t}s: curl exit $rc, created=$(comment_hits "$N" "$u")"
done

echo "== 4. search lag (search must never justify a repost; this only measures it)"
st=$(date +%s)
for _ in $(seq 1 30); do
  a=$(api -G https://api.github.com/search/issues --data-urlencode "q=\"$U\" repo:$R is:issue" | jq .total_count)
  c=$(api -G https://api.github.com/search/issues --data-urlencode "q=\"$UC\" repo:$R in:comments" | jq .total_count)
  echo "$(( $(date +%s) - st ))s body=$a comment=$c"; [ "$a" = 1 ] && [ "$c" = 1 ] && break; sleep 10
done
cat <<'EOF'

Confirms the design if: raw_has=true, html_has=false, the creator listing works with the bot login, and lags are ~0 s.
Any "NOT FOUND" or a non-zero lag means recovery must wait and recheck before declaring a write lost.
Section 3: created=1 with a curl timeout is exactly the lost-response case the marker exists for.
EOF
