#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: scripts/release.sh vMAJOR.MINOR.PATCH [output-directory]" >&2
  exit 2
}

[[ $# -ge 1 && $# -le 2 ]] || usage
tag=$1
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]] || {
  echo "release tag must be a semantic version beginning with v: $tag" >&2
  exit 2
}

version=${tag#v}
output=${2:-dist}
commit=${COMMIT:-$(git rev-parse HEAD)}
build_date=${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
ldflags="-s -w -X main.version=$tag -X main.commit=$commit -X main.buildDate=$build_date"

targets=(
  linux/amd64
  linux/arm64
  darwin/amd64
  darwin/arm64
  windows/amd64
  windows/arm64
)

rm -rf "$output"
mkdir -p "$output"
output=$(cd "$output" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for target in "${targets[@]}"; do
  goos=${target%/*}
  goarch=${target#*/}
  binary=notch
  extension=.tar.gz
  if [[ $goos == windows ]]; then
    binary=notch.exe
    extension=.zip
  fi
  archive="notch_${version}_${goos}_${goarch}${extension}"
  echo "building $target"
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
    go build -trimpath -ldflags "$ldflags" -o "$work/$binary" ./cmd/notch
  if [[ $goos == windows ]]; then
    (cd "$work" && zip -q "$output/$archive" "$binary")
  else
    tar -C "$work" -czf "$output/$archive" "$binary"
  fi
  rm -f "$work/$binary"
done

(
  cd "$output"
  sha256sum notch_* > checksums.txt
)
echo "release artifacts written to $output"
