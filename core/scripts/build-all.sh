#!/usr/bin/env bash
# Build GEX Suite binaries for every supported platform into ../dist.
# Pure Go (modernc SQLite) — one machine cross-compiles all targets, no
# per-OS toolchains, no installers. Users just run the binary.
set -euo pipefail
cd "$(dirname "$0")/.."

OUT="${1:-dist}"
rm -rf "$OUT"
mkdir -p "$OUT"

VERSION="$(date +%Y%m%d)"
LDFLAGS="-s -w"

build() {
  local os="$1" arch="$2" ext="${3:-}"
  local dir="gexsuite-${os}-${arch}"
  echo "→ ${dir}"
  mkdir -p "${OUT}/${dir}"
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" \
    -o "${OUT}/${dir}/gexctl${ext}" ./cmd/gexctl
  cp README-GUI.md "${OUT}/${dir}/" 2>/dev/null || true
}

build windows amd64 .exe
build linux   amd64 ""
build linux   arm64 ""
build darwin  amd64 ""
build darwin  arm64 ""

# tarballs for download/transfer
for d in "${OUT}"/gexsuite-*/; do
  name="$(basename "$d")"
  tar -czf "${OUT}/${name}-${VERSION}.tar.gz" -C "$OUT" "$name"
done

echo
echo "done:"
ls -1 "$OUT"
