#!/usr/bin/env bash
# Spike A: does a read-only App see repo and org rules, and what does `protected` mean? Run once, on a fresh repo.
. "$(dirname "$0")/lib.sh" a
TOKEN=${ADMIN:?}
main=$(api "$B/git/ref/heads/main" | jq -r .object.sha)
for b in plain legacy rs-repo rs-disabled rs-linear rs-org; do
  api -X POST "$B/git/refs" -d "{\"ref\":\"refs/heads/$b\",\"sha\":\"$main\"}" > /dev/null
done
mk(){ rec "setup-$1" -X POST "$B/rulesets" -d "{\"name\":\"$1\",\"target\":\"branch\",\"enforcement\":\"$2\",\"conditions\":{\"ref_name\":{\"include\":[\"refs/heads/$1\"],\"exclude\":[]}},\"rules\":$3}" | show '{id}'; }
mk rs-repo active '[{"type":"deletion"},{"type":"non_fast_forward"}]'
mk rs-disabled disabled '[{"type":"deletion"}]'
mk rs-linear active '[{"type":"required_linear_history"}]'
rec setup-legacy -X PUT "$B/branches/legacy/protection" -d '{"required_status_checks":null,"enforce_admins":null,"required_pull_request_reviews":null,"restrictions":null}' | show '{url}'
# A failure here on a Free plan answers the plan question
rec setup-rs-org -X POST "https://api.github.com/orgs/$ORG/rulesets" -d '{"name":"rs-org","target":"branch","enforcement":"active","conditions":{"ref_name":{"include":["refs/heads/rs-org"],"exclude":[]},"repository_name":{"include":["cove-spike"],"exclude":[]}},"rules":[{"type":"deletion"}]}' | show '{id}'

echo "== read-only App"
TOKEN=$(mint ro)
rec ro-branches "$B/branches" | show '{name,protected}'
rec ro-branches-protected "$B/branches?protected=true" | show '.name'
for b in plain legacy rs-repo rs-disabled rs-linear rs-org; do
  echo "== $b"
  rec "ro-rules-$b" "$B/rules/branches/$b" | show '{type,ruleset_source_type,ruleset_source,ruleset_id}'
  rec "ro-protection-$b" "$B/branches/$b/protection" > /dev/null
  grep -i 'x-accepted-github-permissions' "$FX/ro-protection-$b.headers"; echo
done
rec ro-rulesets "$B/rulesets?includes_parents=true" | show '{id,name,source_type,enforcement}'
rec ro-org-rulesets "https://api.github.com/orgs/$ORG/rulesets" | show '{id,name}'

echo "== admin, for comparison"
TOKEN=$ADMIN
rec admin-protection-rs-repo "$B/branches/rs-repo/protection" | show '{url}'
cat <<'EOF'

Expected: plain false/[]; legacy true/[]; rs-repo true/[deletion,non_fast_forward]; rs-org true/[deletion];
rs-disabled false/[]; rs-linear ?/[required_linear_history]. The read-only App gets 403 on */protection and orgs/*/rulesets.
Refuted if rs-repo shows protected=false, or rs-org's rules are empty.
EOF
