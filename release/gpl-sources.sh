#!/bin/sh
# Collects the source of every GPL-licensed Alpine package in a GitCove image (git, busybox,
# apk-tools and a few more): each package's build recipe and patches from Alpine's aports, and the
# upstream archives it names. The result ships with the release, next to the image, which is how
# GPLv2 section 3(a) is met. Usage: release/gpl-sources.sh IMAGE OUT.tar.gz
set -eu
image=$1 out=$2
ver=$(docker run --rm --entrypoint cat "$image" /etc/alpine-release | cut -d. -f1,2)
installed=$(docker run --rm --entrypoint apk "$image" list -I | grep 'GPL')
origins=$(echo "$installed" | sed 's/.* {\(.*\)} (.*/\1/' | sort -u | tr '\n' ' ')
docker run --rm -i "alpine:$ver" sh -euc '
  apk add -q alpine-sdk >&2
  git clone -q --depth 1 -b '"$ver"'-stable https://gitlab.alpinelinux.org/alpine/aports.git /aports
  mkdir /src && cd /src
  cat > INSTALLED
  for p in '"$origins"'; do
    cp -r /aports/main/$p . && (cd $p && SRCDEST=/src/$p abuild -F fetch >&2)
    grep -E "^pkg(ver|rel)=" $p/APKBUILD | tr "\n" " " | sed "s/^/$p: /; s/\$/\n/" >> VERSIONS
  done
  tar czf - .' <<EOT > "$out"
$installed
EOT
tar xzOf "$out" ./VERSIONS
