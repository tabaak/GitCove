#!/bin/sh
# Writes THIRD_PARTY_NOTICES.md: the licence files of every module linked into cove.
set -eu
cd "$(dirname "$0")/.."
{
  printf '# Third-party notices\n\nThe `cove` binary includes the Go modules below, and the Go standard library (BSD-3-Clause). Regenerate with `release/notices.sh` after changing dependencies. The Docker image also contains Alpine Linux packages, listed with their licences in the image SBOM. The source of the GPL-licensed ones, Git and BusyBox among them, ships with each release as `gitcove-gpl-sources.tar.gz`.\n'
  go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/cove | sort -u | while read -r path ver dir; do
    for f in "$dir"/LICENSE* "$dir"/PATENTS; do
      [ -f "$f" ] || continue
      printf '\n## %s %s: %s\n\n````\n' "$path" "$ver" "$(basename "$f")"
      cat "$f"
      printf '\n````\n'
    done
  done
} > THIRD_PARTY_NOTICES.md
