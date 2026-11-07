#!/usr/bin/env bash
# convert.sh ro|rw CODE: swap the manifest code for the App's id and key, saved in .secrets/
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); S=$HERE/.secrets
mkdir -p "$S" "$HERE/fixtures/apps"; chmod 700 "$S"
k=$1; code=$2
res=$(curl -sS -X POST -H 'Accept: application/vnd.github+json' "https://api.github.com/app-manifests/$code/conversions")
id=$(jq -r .id <<<"$res")
[ "$id" != null ] || { echo "$res"; exit 1; }
jq -r .pem <<<"$res" > "$S/cove-$k.pem"; chmod 600 "$S/cove-$k.pem"
jq 'del(.pem, .client_secret, .webhook_secret)' <<<"$res" > "$HERE/fixtures/apps/conversion-$k.json"
K=$(tr a-z A-Z <<<"$k")
{
  echo "${K}_ID=$id"
  echo "${K}_SLUG=$(jq -r .slug <<<"$res")"
  echo "${K}_CLIENT_ID=$(jq -r .client_id <<<"$res")"
  echo "${K}_CLIENT_SECRET=$(jq -r .client_secret <<<"$res")"
} >> "$S/env"
echo "Saved cove-$k (id $id). Now install it on cove-spike only: $(jq -r .html_url <<<"$res")/installations/new"
