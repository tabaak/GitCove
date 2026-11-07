#!/usr/bin/env bash
# Spike U: can cove-rw act as a person (comment, review, approve) with a user token, how long do tokens last,
# and does that approval count towards branch protection? Before running: in cove-rw's settings, tick
# "Enable Device Flow" (manifests can't set it). The PR is opened by the App, so you can approve it yourself.
. "$(dirname "$0")/lib.sh" u
redact(){ jq 'with_entries(if (.key|test("token$")) then .value="***" else . end)'; }
oauth(){ curl -sS -X POST -H 'Accept: application/json' https://github.com/login/oauth/access_token "$@"; }
decision(){ TOKEN=$ADMIN api -X POST https://api.github.com/graphql \
  -d "{\"query\":\"{repository(owner:\\\"$ORG\\\",name:\\\"cove-spike\\\"){pullRequest(number:$PR){reviewDecision mergeStateStatus}}}\"}" \
  | jq -c '.data.repository.pullRequest // .'; }

echo "== PR opened by the App"
TOKEN=$(mint rw); RW=$TOKEN; fresh_clone "$TOKEN"
git checkout -qb u-test origin/main; echo u > u.txt; git add u.txt; git commit -qm u1; H1=$(git rev-parse HEAD)
grec push-h1 git push origin u-test
PR=$(rec create-pr -X POST "$B/pulls" -d '{"title":"user token test","head":"u-test","base":"main","body":"spike U"}' | jq .number)
echo "PR #$PR head $H1"

echo "== device flow"
DC=$(curl -sS -X POST -H 'Accept: application/json' https://github.com/login/device/code -d "client_id=${RW_CLIENT_ID:?}")
jq -e .device_code <<<"$DC" > /dev/null || { echo "$DC"; echo "Is Device Flow enabled on cove-rw?"; exit 1; }
echo "Open $(jq -r .verification_uri <<<"$DC") and enter $(jq -r .user_code <<<"$DC")"
iv=$(jq .interval <<<"$DC")
while :; do
  sleep "$iv"
  T=$(oauth -d "client_id=$RW_CLIENT_ID" -d "device_code=$(jq -r .device_code <<<"$DC")" -d grant_type=urn:ietf:params:oauth:grant-type:device_code)
  case $(jq -r '.error // "ok"' <<<"$T") in
    ok) break ;; authorization_pending) ;; slow_down) iv=$((iv + 5)) ;; *) echo "$T"; exit 1 ;;
  esac
done
redact <<<"$T" | tee "$FX/device-token.json"
UT=$(jq -r .access_token <<<"$T"); RT=$(jq -r .refresh_token <<<"$T")

echo "== acting as the person"
TOKEN=$UT
rec whoami https://api.github.com/user | jq -c '{login}'
rec user-comment -X POST "$B/issues/$PR/comments" -d '{"body":"comment via user token"}' | jq -c '{user:.user.login,app:.performed_via_github_app.slug}'
rec user-review-comment -X POST "$B/pulls/$PR/reviews" -d '{"event":"COMMENT","body":"review via user token"}' | show '{user:.user.login,state}'

echo "== branch protection: 1 approval, dismiss stale approvals"
TOKEN=${ADMIN:?}
rec setup-protection -X PUT "$B/branches/main/protection" -d '{"required_status_checks":null,"enforce_admins":false,"required_pull_request_reviews":{"required_approving_review_count":1,"dismiss_stale_reviews":true},"restrictions":null}' | show '{url}'
echo "before approval: $(decision)"
TOKEN=$UT
rec user-approve -X POST "$B/pulls/$PR/reviews" -d "{\"event\":\"APPROVE\",\"commit_id\":\"$H1\"}" | show '{user:.user.login,state,commit_id}'
sleep 3; echo "after approval: $(decision)"

echo "== stale approvals"
echo u2 >> u.txt; git commit -qam u2; H2=$(git rev-parse HEAD)
grec push-h2 git push "$(gurl "$RW")" u-test
sleep 5; echo "after new commit: $(decision)"
TOKEN=$UT
rec user-approve-old-commit -X POST "$B/pulls/$PR/reviews" -d "{\"event\":\"APPROVE\",\"commit_id\":\"$H1\"}" | show '{state,commit_id}'
sleep 3; echo "after approving the old commit $H1 while head is $H2: $(decision)"

echo "== refresh"
T2=$(oauth -d "client_id=$RW_CLIENT_ID" -d "client_secret=${RW_CLIENT_SECRET:?}" -d grant_type=refresh_token -d "refresh_token=$RT")
redact <<<"$T2" | tee "$FX/refresh.json"
echo "reusing the old refresh token:"
oauth -d "client_id=$RW_CLIENT_ID" -d "client_secret=$RW_CLIENT_SECRET" -d grant_type=refresh_token -d "refresh_token=$RT" | redact | tee "$FX/refresh-reuse.json"
cat <<'EOF'

Record: expires_in and refresh_token_expires_in (expected 28800 and 15897600), whether comments show the person
with the App badge, whether reviewDecision becomes APPROVED, what the old-commit approval does, and whether
the old refresh token is rejected after use.
EOF
