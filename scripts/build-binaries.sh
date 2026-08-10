#!/usr/bin/env bash
#
# Cross-compiles the QueryForge engine for every supported platform.
#
# Go cross-compiles from any host with no toolchain to install, which is the whole reason the
# engine is written in Go and the SDKs are thin: one machine produces every artifact the release
# needs, and none of them require a C compiler or a matching runner.
#
# Output layout, which the SDK packaging depends on:
#
#   dist/
#     linux-amd64/queryforge
#     linux-arm64/queryforge
#     darwin-amd64/queryforge
#     darwin-arm64/queryforge
#     windows-amd64/queryforge.exe
#     checksums.txt
#
# Those directory names are the platform tags BinaryResolver (Java) and _binary.py (Python)
# compute at runtime, so a rename here has to be made in three places at once.
#
# Usage:
#   scripts/build-binaries.sh [version] [output-dir]

set -euo pipefail

VERSION="${1:-dev}"
OUT_DIR="${2:-dist}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# The platforms the release promises. Each entry is "tag:GOOS:GOARCH".
PLATFORMS=(
  "linux-amd64:linux:amd64"
  "linux-arm64:linux:arm64"
  "darwin-amd64:darwin:amd64"
  "darwin-arm64:darwin:arm64"
  "windows-amd64:windows:amd64"
)

# -s -w strips the symbol table and DWARF debug info, roughly halving each binary. These are
# release artifacts bundled into every SDK install, so size is a user-visible cost; a stack trace
# from a stripped Go binary still carries function names, which is what a bug report needs.
#
# main.Version is stamped here rather than read from a file, so `queryforge --version` and the
# `version` op report the release they were actually built from.
LDFLAGS="-s -w -X main.Version=${VERSION}"

echo "Building QueryForge ${VERSION} into ${OUT_DIR}/"
rm -rf "${OUT_DIR:?}"
mkdir -p "$OUT_DIR"

for entry in "${PLATFORMS[@]}"; do
  IFS=':' read -r tag goos goarch <<< "$entry"

  binary="queryforge"
  if [ "$goos" = "windows" ]; then
    binary="queryforge.exe"
  fi

  target_dir="${OUT_DIR}/${tag}"
  mkdir -p "$target_dir"

  echo "  ${tag} (GOOS=${goos} GOARCH=${goarch})"
  # CGO_ENABLED=0 produces a static binary with no libc dependency. Without it, a binary built on
  # a modern glibc refuses to start on an older one — the single most common way a bundled native
  # binary fails, and one that only shows up on a user's machine.
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "${target_dir}/${binary}" ./cmd/queryforge

  chmod +x "${target_dir}/${binary}"
done

# One checksum file over every artifact. It is what lets a user verify a download, and what lets
# CI prove the binary a wheel shipped is the binary that was built.
(
  cd "$OUT_DIR"
  if command -v sha256sum >/dev/null 2>&1; then
    find . -type f ! -name checksums.txt -exec sha256sum {} + | sort -k2 > checksums.txt
  else
    # macOS ships shasum rather than sha256sum; match the sha256sum output format so the file is
    # verifiable with either tool.
    find . -type f ! -name checksums.txt -exec shasum -a 256 {} + | sort -k2 > checksums.txt
  fi
)

echo
echo "Built $(find "$OUT_DIR" -type f ! -name checksums.txt | wc -l | tr -d ' ') binaries:"
ls -la "$OUT_DIR"/*/ | grep queryforge || true

# A smoke test on the host's own binary. Cross-compiled artifacts cannot be run here, but the one
# matching this machine can — and a binary that cannot answer `version` would otherwise ship.
HOST_OS="$(go env GOOS)"
HOST_ARCH="$(go env GOARCH)"
HOST_TAG="${HOST_OS}-${HOST_ARCH}"
if [ -x "${OUT_DIR}/${HOST_TAG}/queryforge" ]; then
  echo
  echo "Smoke-testing ${HOST_TAG}:"
  echo '{"op":"version"}' | "${OUT_DIR}/${HOST_TAG}/queryforge"
fi
