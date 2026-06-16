#!/usr/bin/env bash
# Build the Debian/Ubuntu package for one architecture.
#
#   packaging/deb/build.sh <amd64|arm64>
#
# Needs go and nfpm on PATH (locally: `mise x -- packaging/deb/build.sh ...`).
# DEB_VERSION overrides the package version (release CI passes the tag);
# dev builds default to 0.0.0~r<count>.<hash>, which sorts before any
# release. Output lands in packaging/deb/dist/.
set -euo pipefail

arch=${1:?usage: build.sh <amd64|arm64>}
case "$arch" in
amd64 | arm64) ;;
*)
  echo "unsupported arch: $arch (want amd64 or arm64)" >&2
  exit 1
  ;;
esac

repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
cd "$repo"

version=${DEB_VERSION:-"0.0.0~r$(git rev-list --count HEAD).$(git rev-parse --short HEAD)"}

staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT

# -s -w strips the ELF symbol/DWARF tables (lintian: unstripped-binary);
# panic backtraces are unaffected (Go resolves frames via pclntab).
CGO_ENABLED=0 GOARCH="$arch" go build -trimpath -ldflags '-s -w' \
  -o "$staging/github-qemu-runner" ./cmd/github-qemu-runner

# The shared unit targets the manual-install path (/usr/local/bin); the
# packaged binary lives in /usr/bin (same transform as the PKGBUILDs).
sed 's|/usr/local/bin/github-qemu-runner|/usr/bin/github-qemu-runner|' \
  packaging/github-qemu-runner.service >"$staging/github-qemu-runner.service"

# Same /usr/local/bin -> /usr/bin rewrite for the refresh service. The
# timer carries no exec path, so nfpm ships it verbatim from packaging/.
sed 's|/usr/local/bin/github-qemu-runner|/usr/bin/github-qemu-runner|' \
  packaging/github-qemu-runner-refresh.service >"$staging/github-qemu-runner-refresh.service"

# Plain-text copyright file (LICENSE verbatim) and a one-stanza
# changelog.gz — the minimum /usr/share/doc lintian expects.
cp LICENSE "$staging/copyright"
cat >"$staging/changelog" <<EOF
github-qemu-runner ($version) unstable; urgency=medium

  * See https://github.com/a1678991/github-qemu-runner/blob/main/CHANGELOG.md

 -- a1678991 <a1678991@iroserver.net>  $(date -R -u)
EOF
gzip -9n "$staging/changelog"

mkdir -p packaging/deb/dist
DEB_ARCH="$arch" DEB_VERSION="$version" DEB_STAGING="$staging" \
  nfpm package -f packaging/deb/nfpm.yaml -p deb -t packaging/deb/dist/
