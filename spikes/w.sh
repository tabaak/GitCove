#!/usr/bin/env bash
# Spike W: does GitHub check pushed commits, or the whole tree, for .github/workflows changes?
#   ./w.sh        all cases, with cove-rw lacking the workflows permission
#   ./w.sh after  repeat case 1 and the PUT once cove-rw has Workflows: write
. "$(dirname "$0")/lib.sh" w
wf(){ mkdir -p .github/workflows; printf 'on: workflow_dispatch\n# %s\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n' "$1" > .github/workflows/w.yml; git add -A; git commit -qm "wf $1"; }
put(){ rec "$1" -X PUT "$B/contents/.github/workflows/$2.yml" -d "{\"message\":\"$2\",\"content\":\"b246IHdvcmtmbG93X2Rpc3BhdGNoCg==\"}" | show '{commit:.commit.sha}'; grep -i 'x-accepted-github-permissions' "$FX/$1.headers"; echo; }

if [ "${1:-}" = after ]; then
  TOKEN=$(mint rw); fresh_clone "$TOKEN"
  git checkout -qb t1-after origin/main; wf after; grec case1-after git push origin t1-after
  put rest-put-after z2
  exit
fi

fresh_clone "${ADMIN:?}"
wf v1; A=$(git rev-parse HEAD); git push -q origin HEAD:main
git checkout -qb on-side "$A"; wf side; S2=$(git rev-parse HEAD); git push -q origin on-side
git checkout -q main; wf v2; C=$(git rev-parse HEAD); git push -q origin main
TOKEN=$(mint rw); git remote add app "$(gurl "$TOKEN")"
echo "A=$A (old main, workflow v1)  C=$C (main tip, v2)  S=$S2 (side branch)"

echo "== git push cases"
git checkout -qb t1 "$C"; wf new;                                     grec case1-new-workflow-commit   git push app t1
                                                                      grec case2-branch-at-tip          git push app "$C:refs/heads/t2"
                                                                      grec case3-branch-at-old-commit   git push app "$A:refs/heads/t3"
git checkout -qb t4 "$A"; git commit -q --allow-empty -m x;          grec case4-new-commit-on-old-base git push app t4
                                                                      grec case5-commit-on-other-branch git push app "$S2:refs/heads/t5"
git checkout -qb t6 "$C"; echo x > f; git add f; git commit -qm x;   grec case6-ordinary-commit        git push app t6

echo "== REST cases"
ref(){ rec "$1" -X POST "$B/git/refs" -d "{\"ref\":\"refs/heads/$2\",\"sha\":\"$3\"}" | show '{ref}'; }
ref rest-t7-at-tip t7 "$C"
ref rest-t8-old-commit t8 "$A"
ref rest-t9-other-branch t9 "$S2"
put rest-put-workflow z
cat <<'EOF'

Expected: case 1 and the PUT rejected ("refusing to allow a GitHub App to create or update workflow"); 2 and 6 accepted.
Cases 3, 4 (the failover case), 5, t8 and t9 are the unknowns: any rejection there means the check is tree-based.
Next: give cove-rw Workflows: write, accept the change as owner, then run ./w.sh after.
EOF
