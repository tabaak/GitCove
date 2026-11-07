# Shared helpers for the milestone 0 spikes. Each spike starts with:  . "$(dirname "$0")/lib.sh" <spike>
set -uo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
S=$HERE/.secrets
[ -f "$S/env" ] && . "$S/env"
: "${ORG:?put ORG=<sandbox org> in spikes/.secrets/env}"
R=$ORG/cove-spike
B=https://api.github.com/repos/$R
SPIKE=${1:?spike name}
FX=$HERE/fixtures/$SPIKE
WORK=$HERE/work/$SPIKE
mkdir -p "$FX" "$WORK"
exec > >(tee -a "$FX/run.log") 2>&1
echo "### $SPIKE run at $(date -u +%FT%TZ)"

b64(){ openssl base64 -A | tr '+/' '-_' | tr -d '='; }

# mint ro|rw: print a fresh installation token for cove-ro or cove-rw
mint(){
  local id now h p s jwt iid A='Accept: application/vnd.github+json'
  if [ "$1" = ro ]; then id=$RO_ID; else id=$RW_ID; fi
  now=$(date +%s)
  h=$(printf '{"alg":"RS256","typ":"JWT"}' | b64)
  p=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' $((now-60)) $((now+540)) "$id" | b64)
  s=$(printf '%s.%s' "$h" "$p" | openssl dgst -sha256 -sign "$S/cove-$1.pem" | b64)
  jwt=$h.$p.$s
  iid=$(curl -sS -H "Authorization: Bearer $jwt" -H "$A" "https://api.github.com/orgs/$ORG/installation" | jq -r .id)
  curl -sS -X POST -H "Authorization: Bearer $jwt" -H "$A" "https://api.github.com/app/installations/$iid/access_tokens" | jq -r .token
}

api(){ curl -sS -H "Authorization: Bearer $TOKEN" -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' "$@"; }

# rec NAME curl-args...: like api, but saves the request args, response headers and body as a fixture; prints the body
rec(){
  local n=$1; shift
  printf '%s\n' "$*" > "$FX/$n.req"
  api -D "$FX/$n.headers" -o "$FX/$n.body" "$@"
  printf '%s ' "$(head -1 "$FX/$n.headers" | tr -d '\r')" >&2
  cat "$FX/$n.body"
}

# show JQ-EXPR: compact view of an array response, or the error message
show(){ jq -c "if type==\"array\" then map($1) elif .message then {message, status} else $1 end"; }

# grec NAME cmd...: run a git command, save exit code and output (tokens redacted) as a fixture
grec(){
  local n=$1 out rc; shift
  out=$("$@" 2>&1); rc=$?
  printf 'exit %s\n%s\n' "$rc" "$out" | sed -E 's/x-access-token:[^@]*@/x-access-token:***@/g' | tee "$FX/$n.git.txt"
  return $rc
}

gurl(){ echo "https://x-access-token:$1@github.com/$R.git"; }

# fresh_clone TOKEN: clone cove-spike into $WORK/cove-spike and cd into it
fresh_clone(){
  rm -rf "$WORK/cove-spike"
  git clone -q "$(gurl "$1")" "$WORK/cove-spike" && cd "$WORK/cove-spike" || exit 1
  git config user.name cove-spike; git config user.email cove-spike@example.invalid
}
