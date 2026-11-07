#!/usr/bin/env bash
# Spike I: inline review comments anchored to an older commit, and to a commit GitHub has never seen.
. "$(dirname "$0")/lib.sh" i
TOKEN=$(mint rw); fresh_clone "$TOKEN"
git checkout -qb inline-test origin/main
printf 'line1\nline2\nline3\n' > i.txt; git add i.txt; git commit -qm c1; C1=$(git rev-parse HEAD)
grec push-c1 git push origin inline-test
PR=$(rec create-pr -X POST "$B/pulls" -d '{"title":"inline test","head":"inline-test","base":"main","body":"spike I"}' | jq .number)
printf 'line1\nline2 changed\nline3\n' > i.txt; git commit -qam c2; C2=$(git rev-parse HEAD)
grec push-c2 git push origin inline-test
echo "PR #$PR  c1=$C1  c2=$C2 (head)"

c(){ rec "$1" -X POST "$B/pulls/$PR/comments" -d "{\"body\":\"$1\",\"commit_id\":\"$2\",\"path\":\"i.txt\",\"line\":2,\"side\":\"RIGHT\"}" | show '{id,commit_id,original_commit_id,line,position}'; }
c comment-on-old-commit "$C1"
c comment-on-head "$C2"
rec review-on-old-commit -X POST "$B/pulls/$PR/reviews" -d "{\"commit_id\":\"$C1\",\"event\":\"COMMENT\",\"body\":\"review on c1\",\"comments\":[{\"path\":\"i.txt\",\"line\":2,\"side\":\"RIGHT\",\"body\":\"line comment in review on c1\"}]}" | show '{id,commit_id,state}'

echo "== commits GitHub has never seen"
git commit -q --allow-empty -m never-pushed; c comment-on-unpushed-commit "$(git rev-parse HEAD)"
c comment-on-random-sha 0123456789abcdef0123456789abcdef01234567
git reset -q --hard "$C2"

echo "== move the branch so the earlier comments go stale"
printf 'line1\nline2 changed again\nline3\n' > i.txt; git commit -qam c3
grec push-c3 git push origin inline-test
sleep 3
rec comments-after-move "$B/pulls/$PR/comments" | show '{body,commit_id,original_commit_id,line,original_line,position,original_position}'
echo "Now look at https://github.com/$R/pull/$PR/files and the Conversation tab, and note how each comment is shown."
