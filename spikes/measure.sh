#!/usr/bin/env bash
# Milestone 7 measurements with the real cove binary against the sandbox organisation:
#   ./measure.sh delay    sync delay with polling and with webhook hints (about 70 minutes), and API calls per repo per hour
#   ./measure.sh hook     the webhook part of delay alone (about 3 minutes)
#   ./measure.sh bulk     backup time for a 100 MB repo, and replay time for 100 comments and 5 branches
# API calls are counted by a local proxy in front of api.github.com. Webhook deliveries are signed
# and sent from this machine, so GitHub's own delivery delay isn't in the webhook figures.
. "$(dirname "$0")/lib.sh" measure
set -e
ROOT=$(cd "$HERE/.." && pwd)
D=$WORK/$(date -u +%m%d%H%M%S)
mkdir -p "$D"
PORT=18474 PROXY=18475
COVE=$D/cove
(cd "$ROOT" && go build -o "$COVE" ./cmd/cove)
echo measure-secret > "$D/hook-secret"
export COVE_ORG=$ORG COVE_APP_ID=$RW_ID COVE_APP_KEY_FILE=$S/cove-rw.pem COVE_DATA=$D/data \
  COVE_LISTEN=127.0.0.1:$PORT COVE_GITHUB_API=http://127.0.0.1:$PROXY COVE_WEBHOOK_SECRET_FILE=$D/hook-secret \
  COVE_REPLAY_INTERVAL=5s GIT_TERMINAL_PROMPT=0
U=http://127.0.0.1:$PORT
TOKEN=$(mint rw)
now(){ python3 -c 'import time; print(time.time())'; }
since(){ python3 -c "import time; print(round(time.time()-$1, 1))"; }
stats(){ python3 -c 'import sys,statistics as s; x=sorted(map(float,sys.stdin)); print(f"n={len(x)} median={s.median(x):.1f}s p95={x[max(0,-(-len(x)*95//100)-1)]:.1f}s max={x[-1]:.1f}s")'; }

# The proxy logs one line per API request: time, method, status, path.
python3 - "$PROXY" "$D/api.log" <<'EOF' &
import http.server, sys, time, urllib.request, urllib.error
port, log = int(sys.argv[1]), open(sys.argv[2], "a", buffering=1)
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0)) or None
        hdrs = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "accept-encoding")}
        try:
            r = urllib.request.urlopen(urllib.request.Request("https://api.github.com" + self.path, body, hdrs, method=self.command))
        except urllib.error.HTTPError as e:
            r = e
        data = r.read()
        log.write(f"{time.time():.0f} {self.command} {r.status} {self.path.split('?')[0]}\n")
        self.send_response(r.status)
        for k, v in r.headers.items():
            if k.lower() not in ("transfer-encoding", "content-length", "connection"):
                self.send_header(k, v.replace("https://api.github.com", f"http://127.0.0.1:{port}"))  # paging links
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    do_POST = do_PATCH = do_PUT = do_DELETE = do_GET
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
EOF
PROXYPID=$!
trap 'kill $PROXYPID $SRV 2>/dev/null' EXIT
sleep 1
serve(){ "$COVE" serve >>"$D/serve.log" 2>&1 & SRV=$!; until curl -fsS $U/status >/dev/null 2>&1; do sleep 0.2; done; }
# commit BRANCH: a commit on GitHub through the API; prints its ID
commit(){ api -X PUT "$B/contents/measure-$RANDOM$RANDOM.txt" -d "{\"message\":\"measure\",\"content\":\"eAo=\",\"branch\":\"$1\"}" | jq -r .commit.sha; }
# served BRANCH SHA: wait until GitCove serves the commit
served(){ until git ls-remote "http://x:$TOK@127.0.0.1:$PORT/$R.git" "refs/heads/$1" | grep -q "$2"; do sleep 0.5; done; }

case ${1:?delay, hook or bulk} in
delay|hook)
  export COVE_SYNC_INTERVAL=5m
  "$COVE" sync
  TOK=$("$COVE" token create -name measure -repo "$R")
  MAIN=$(api "$B/git/ref/heads/main" | jq -r .object.sha)
  api -X POST "$B/git/refs" -d "{\"ref\":\"refs/heads/sb-measure\",\"sha\":\"$MAIN\"}" >/dev/null
  serve
  if [ "$1" = delay ]; then
  start=$(date +%s)
  echo "## polling every 5m: 40 new branches on GitHub, one every 97 s, so they fall all over the sync cycle"
  for i in $(seq 40); do
    ( TOKEN=$(mint rw)
      api -X POST "$B/git/refs" -d "{\"ref\":\"refs/heads/sb-measure-$i\",\"sha\":\"$MAIN\"}" >/dev/null; t=$(now)
      until git ls-remote "http://x:$TOK@127.0.0.1:$PORT/$R.git" "refs/heads/sb-measure-$i" | grep -q .; do sleep 0.5; done
      since "$t" >> "$D/poll" ) &
    sleep 97
  done
  until [ "$(grep -c . "$D/poll")" = 40 ]; do sleep 2; done
  cat "$D/poll" | tr '\n' ' '; echo
  stats < "$D/poll"
  hours=$(python3 -c "print(($(date +%s)-$start)/3600)")
  repos=$(curl -fsS $U/status | jq .repos)
  python3 - "$D/api.log" "$start" "$hours" "$repos" <<'EOF'
import sys, collections
log, start, hours, repos = sys.argv[1], float(sys.argv[2]), float(sys.argv[3]), int(sys.argv[4])
rows = [l.split() for l in open(log) if float(l.split()[0]) >= start]
by = collections.Counter(r[2] for r in rows)
print(f"API requests over {hours:.2f} h with {repos} repos: {len(rows)} ({dict(by)}), "
      f"{len(rows)/hours/repos:.0f} per repo per hour, {(len(rows)-by['304'])/hours/repos:.0f} not counting 304s")
EOF
  fi
  echo "## webhook hints: 20 commits, each followed by a signed delivery"
  TOKEN=$(mint rw) # the polling part outlives the first token
  ID=$(api "$B" | jq .id)
  for i in $(seq 20); do
    sha=$(commit sb-measure); t=$(now)
    body="{\"repository\":{\"id\":$ID}}"
    sig=$(printf %s "$body" | openssl dgst -sha256 -hmac measure-secret | sed 's/.* //')
    curl -fsS -o /dev/null -H "X-Hub-Signature-256: sha256=$sig" -H "X-GitHub-Delivery: measure-$sha" -d "$body" $U/webhook
    served sb-measure "$sha"; since "$t" | tee -a "$D/hook"
    sleep 5
  done
  stats < "$D/hook"
  TOKEN=$(mint rw)
  for i in "" $(seq -f '-%g' 40); do api -X DELETE "$B/git/refs/heads/sb-measure$i" >/dev/null; done
  ;;
bulk)
  R2=$ORG/cove-test
  echo "## a 100 MB repo: 200 commits of 500 KB of incompressible data"
  git init -q "$D/bulk" && cd "$D/bulk"
  git config user.name measure; git config user.email measure@example.invalid
  for i in $(seq 200); do head -c 375000 /dev/urandom | base64 > "f$((i % 20)).txt"; git add .; git commit -qm "bulk $i"; done
  t=$(now); git push -q "https://x-access-token:$TOKEN@github.com/$R2.git" HEAD:refs/heads/sb-bulk; echo "pushed to GitHub in $(since "$t") s"
  cd "$HERE"
  t=$(now); "$COVE" sync; echo "first sync (backup) of every repo took $(since "$t") s; data is $(du -sh "$D/data" | cut -f1)"
  t=$(now); "$COVE" sync; echo "second sync, nothing changed: $(since "$t") s"
  curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" "https://api.github.com/repos/$R2/git/refs/heads/sb-bulk"

  echo "## replay of 100 comments and 5 new branches"
  TOK=$("$COVE" token create -name measure -repo "$R")
  ISSUE=$(api -X POST "$B/issues" -d '{"title":"sb-measure replay"}' | jq -r .number)
  sleep 10; "$COVE" sync
  export COVE_SYNC_INTERVAL=5m
  serve
  "$COVE" failover start
  git clone -q "http://x:$TOK@127.0.0.1:$PORT/$R.git" "$D/dev" && cd "$D/dev"
  git config user.name measure; git config user.email measure@example.invalid
  for i in 1 2 3 4 5; do
    git checkout -q -b "sb-measure-$(basename "$D")-$i" origin/main; git commit -q --allow-empty -m "measure $i"; git push -q origin HEAD 2>/dev/null
  done
  cd "$HERE"
  for i in $(seq 100); do curl -fsS -o /dev/null -u "x:$TOK" --data-urlencode "body=measure $i" "$U/$R/issues/$ISSUE/comment"; done
  "$COVE" failover end >/dev/null
  t=$(now); "$COVE" replay approve
  until [ "$(curl -fsS $U/status | jq -r .mode)" = normal ]; do sleep 1; done
  echo "replay took $(since "$t") s"
  TOKEN=$(mint rw)
  echo "comments on GitHub: $(api "$B/issues/$ISSUE" | jq .comments) (want 100)"
  ;;
esac
