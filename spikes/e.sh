#!/usr/bin/env bash
# Spike E: what a read-only App can sync (collaborators, members, refs/pull), and whether App PRs trigger CI.
# Optional in .secrets/env: SPIKE_USER (a collaborator's login), SPIKE_TEAM (a team slug).
. "$(dirname "$0")/lib.sh" e

echo "== part 1: read-only App"
TOKEN=$(mint ro)
paths=("repos/$R/collaborators" "orgs/$ORG/members" "orgs/$ORG/outside_collaborators" "orgs/$ORG/teams" "repos/$R/teams" "repos/$R/issues" "repos/$R/pulls")
[ -n "${SPIKE_USER:-}" ] && paths+=("repos/$R/collaborators/$SPIKE_USER/permission")
[ -n "${SPIKE_TEAM:-}" ] && paths+=("orgs/$ORG/teams/$SPIKE_TEAM/members" "orgs/$ORG/teams/$SPIKE_TEAM/repos")
for p in "${paths[@]}"; do
  n=ro-$(tr '/' '_' <<<"$p")
  printf '%s: ' "$p"; rec "$n" "https://api.github.com/$p" | jq -c 'if type=="array" then {count:length} else {message, permission} end'
  grep -i 'x-accepted-github-permissions' "$FX/$n.headers"
done
grec ls-remote-pull git ls-remote "$(gurl "$TOKEN")" 'refs/pull/*'
rm -rf "$WORK/cove.git"; git init -q --bare "$WORK/cove.git"
grec fetch-pull git -C "$WORK/cove.git" fetch "$(gurl "$TOKEN")" '+refs/pull/*:refs/gitcove/github/pull/*' '+refs/heads/*:refs/gitcove/github/heads/*'
SHA=$(git -C "$WORK/cove.git" for-each-ref --format='%(objectname)' refs/gitcove | head -1)
grec ro-push-should-fail git -C "$WORK/cove.git" push "$(gurl "$TOKEN")" "$SHA:refs/heads/ro-should-fail"

echo "== part 2: does an App-created PR trigger CI?"
TOKEN=${ADMIN:?}
rec add-workflow -X PUT "$B/contents/.github/workflows/t.yml" -d "{\"message\":\"add t.yml\",\"content\":\"$(printf 'on: [push, pull_request]\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n' | base64 | tr -d '\n')\"}" | show '{commit:.commit.sha}'
TOKEN=$(mint rw); fresh_clone "$TOKEN"
git checkout -qb app-pr origin/main; echo y > y; git add y; git commit -qm y; SHA=$(git rev-parse HEAD)
grec push-app-pr git push origin app-pr
rec create-pr -X POST "$B/pulls" -d '{"title":"app pr","head":"app-pr","base":"main","body":"x"}' | jq -c '{number,user:.user.login}'
echo "waiting 60 s for runs"; sleep 60
TOKEN=$ADMIN
rec runs "$B/actions/runs?head_sha=$SHA" | jq -c '.workflow_runs[]|{event,status,conclusion}'
cat <<'EOF'

Part 1 confirms read-only sync if collaborators returns a list, org endpoints return 200, repos/*/teams is 403,
the refs/pull fetch works, and the push fails with a permission error (not "src refspec").
Part 2 confirms if there are both push and pull_request runs, neither "action_required".
EOF
