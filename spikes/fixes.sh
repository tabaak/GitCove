#!/usr/bin/env bash
# Does a PR opened by the App with "Fixes #N" close issue N when merged? Run before u.sh adds branch protection.
. "$(dirname "$0")/lib.sh" fixes
TOKEN=$(mint rw); fresh_clone "$TOKEN"
N=$(rec create-issue -X POST "$B/issues" -d '{"title":"fixes target"}' | jq .number)
git checkout -qb fixes-test origin/main; echo fix > fix.txt; git add fix.txt; git commit -qm fix
grec push git push origin fixes-test
PR=$(rec create-pr -X POST "$B/pulls" -d "{\"title\":\"fixes test\",\"head\":\"fixes-test\",\"base\":\"main\",\"body\":\"Replayed from GitCove.\\n\\nFixes #$N\"}" | jq .number)
sleep 3
rec merge -X PUT "$B/pulls/$PR/merge" -d '{"merge_method":"merge"}' | show '{merged,sha}'
sleep 5
rec issue-after "$B/issues/$N" | jq -c '{state,state_reason,closed_by:.closed_by.login}'
rec issue-timeline "$B/issues/$N/timeline" | show 'select(.event=="closed" or .event=="cross-referenced" or .event=="connected")|{event,commit_id,actor:.actor.login}'
echo "Confirms if issue #$N is closed by the merge."
