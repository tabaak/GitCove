#!/bin/sh
# Writes THIRD_PARTY_NOTICES.md: the licence files of every module linked into cove.
set -eu
cd "$(dirname "$0")/.."
{
  printf '# Third-party notices\n\nThe `cove` binary includes the Go modules below, and the Go standard library (BSD-3-Clause). Regenerate with `release/notices.sh` after changing dependencies. The Docker image also contains Alpine Linux packages, listed with their licences in the image SBOM. The source of the GPL-licensed ones, Git and BusyBox among them, ships with each release as `gitcove-gpl-sources.tar.gz`.\n'
  # Octicons are copied as SVG into internal/web/templates.html, so go list doesn't see them.
  printf '\n## Octicons (github.com/primer/octicons): LICENSE\n\n````\nMIT License\n\nCopyright (c) 2023 GitHub Inc.\n\nPermission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:\n\nThe above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.\n\nTHE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.\n````\n'
  go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/cove | sort -u | while read -r path ver dir; do
    for f in "$dir"/LICENSE* "$dir"/COPYING "$dir"/PATENTS; do
      [ -f "$f" ] || continue
      printf '\n## %s %s: %s\n\n````\n' "$path" "$ver" "$(basename "$f")"
      cat "$f"
      printf '\n````\n'
    done
  done
} > THIRD_PARTY_NOTICES.md
