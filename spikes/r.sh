#!/usr/bin/env bash
# Spike R: where does the content-write limit trip for comments? Run LAST, on the sandbox only, with the repo muted.
# GitHub warns that carrying on while limited can get an integration banned, so this stops at the first non-201.
. "$(dirname "$0")/lib.sh" r
TOKEN=$(mint rw)
N=$(rec create-issue -X POST "$B/issues" -d '{"title":"rate probe"}' | jq .number)
st=$(date +%s)
for i in $(seq 1 520); do
  [ $((i % 300)) = 0 ] && TOKEN=$(mint rw)   # tokens last an hour
  code=$(api -o "$WORK/o" -D "$WORK/h" -w '%{http_code}' -X POST "$B/issues/$N/comments" -d "{\"body\":\"probe $i\"}")
  if [ "$code" != 201 ]; then
    echo "STOP at write $i after $(( $(date +%s) - st ))s, http $code"
    cp "$WORK/h" "$FX/blocked.headers"; cp "$WORK/o" "$FX/blocked.body"; cat "$WORK/h" "$WORK/o"
    ra=$(grep -i '^retry-after:' "$WORK/h" | tr -dc 0-9)
    echo "retry-after: ${ra:-absent}"
    wait=${ra:-60}; echo "waiting ${wait}s, then one single write"; sleep "$wait"
    rec after-wait -X POST "$B/issues/$N/comments" -d '{"body":"after wait"}' > /dev/null; echo
    exit
  fi
  [ $((i % 50)) = 0 ] && { echo "$i ok"; cp "$WORK/h" "$FX/ok-$i.headers"; }
  sleep 1
done
echo "all 520 succeeded in $(( $(date +%s) - st ))s: not enforced at this count on $(date -u +%F)"
