#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
tool=go@1.26.10-mihomo-v1.19.33
if ! mise exec "$tool" -- go version >/dev/null 2>&1; then
  mkdir -p .toolchain
  curl -fL --retry 2 https://github.com/MetaCubeX/mihomo/releases/download/v1.19.33/toolchain.tar.gz -o .toolchain/toolchain.tar.gz
  echo 'a827640a1ae37475583ed82eb762ed563d40a4eeb83461acfb5ee8ff10fe4a55  .toolchain/toolchain.tar.gz' | sha256sum -c -
  tar -xzf .toolchain/toolchain.tar.gz -C .toolchain
  mise link "$tool" "$PWD/.toolchain/go"
fi
mkdir -p dist/validation
version=v1.19.33-smart.1
buildtime="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOTOOLCHAIN=local GOOS=linux GOARCH="$arch" GOAMD64=v1 \
  mise exec "$tool" -- go build -tags with_gvisor -trimpath \
    -ldflags "-X github.com/metacubex/mihomo/constant.Version=$version -X github.com/metacubex/mihomo/constant.BuildTime=$buildtime -w -s -buildid=" \
    -o "dist/mihomo-smart-linux-$arch" .
done
(
  cd dist
  sha256sum mihomo-smart-linux-* > SHA256SUMS
)
{
  printf 'upstream_tag=v1.19.33\nupstream_commit=3f27ef39f8c96bbfbc8b0b9bb10f5c9ef60149d8\n'
  printf 'version=%s\nbuilt_at=%s\n' "$version" "$buildtime"
  mise exec "$tool" -- go version
  printf 'toolchain_archive_sha256=a827640a1ae37475583ed82eb762ed563d40a4eeb83461acfb5ee8ff10fe4a55\n'
  printf 'tags=with_gvisor\nCGO_ENABLED=0\nGOAMD64=v1\n'
} > dist/BUILD-INFO.txt
