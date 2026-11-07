#!/usr/bin/env bash
# Create the private repo cove-spike in the sandbox org, using the admin token.
. "$(dirname "$0")/lib.sh" setup
TOKEN=${ADMIN:?put ADMIN=<classic PAT> in .secrets/env}
rec create-repo -X POST "https://api.github.com/orgs/$ORG/repos" -d '{"name":"cove-spike","private":true,"auto_init":true}' | jq -c '{full_name,private,message}'
rec org-plan "https://api.github.com/orgs/$ORG" | jq -c '{login,plan:.plan.name}'
