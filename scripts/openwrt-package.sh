#!/usr/bin/env bash
# Build this repository's OpenWrt package with the official SDK container
# (docker.io/openwrt/sdk) for one package architecture and one OpenWrt release.
#
#   scripts/openwrt-package.sh <arch> <openwrt-release> [version]
#
#   arch             package architecture, e.g. mipsel_24kc (MT7621) or x86_64;
#                    openwrt/sdk.env lists what releases are built for
#   openwrt-release  24.10.x builds an .ipk (opkg), 25.12.x an .apk (apk-tools)
#   version          release version; default: the tag at HEAD or SOURCE_REF
#                    (v1.2.3-rc.4 → 1.2.3-rc.4), else PERCH_VERSION from the
#                    package Makefile. The package inside is 1.2.3~rc4 (.ipk)
#                    or 1.2.3_rc4 (.apk), see below.
#
# The source is this checkout (git archive of HEAD, plus uncommitted changes to
# tracked files) or SOURCE_REF, placed in the SDK's download directory, so the
# package is built from exactly that tree and nothing is fetched from GitHub.
# The packaging (openwrt/, this script) always comes from the checkout, which
# lets a newer checkout package an older tag.
#
# Result: out/openwrt/<package>_<version>-r<release>_<arch>.{ipk,apk}, named
# with the release version (perch-x_1.2.3-rc.4-r1_x86_64.ipk); the
# feeds and build logs stay in out/openwrt/work/<arch>-<release>/bin/.
#
# Environment:
#   OPENWRT_DL    download cache shared between builds (default out/openwrt/dl)
#   GO_BOOTSTRAP  a Go installation (GOROOT, Go >= 1.22) that bootstraps the
#                 SDK's host Go instead of its own bootstrap chain, which saves
#                 minutes per build; default: `go env GOROOT` when Go is installed
#   SOURCE_REF    git ref to package instead of the working tree, e.g. refs/tags/v0.1.0
#   SDK_IMAGE     image override (default docker.io/openwrt/sdk:<arch>-<release>)
#   JOBS          parallel make jobs inside the SDK (default: nproc)
set -euo pipefail
cd "$(dirname "$0")/.."

PKG=perch-apd
usage="usage: $0 <arch> <openwrt-release> [version]"
ARCH="${1:?$usage}"
RELEASE="${2:?$usage}"
VERSION="${3:-}"

case "$RELEASE" in
  24.10.*) EXT=ipk ;;
  25.12.*) EXT=apk ;;
  *) echo "openwrt-package: OpenWrt $RELEASE is not supported (24.10.x or 25.12.x)" >&2; exit 1 ;;
esac

if [ -z "$VERSION" ]; then
  if tag=$(git describe --tags --exact-match "${SOURCE_REF:-HEAD}" 2>/dev/null); then
    VERSION="$tag"
  else
    VERSION=$(sed -n 's/^PERCH_VERSION:=//p' "openwrt/$PKG/Makefile")
  fi
fi
# UPSTREAM is the tag's version (1.0.0-rc.2); package forms of it are accepted too.
UPSTREAM=$(printf '%s' "$VERSION" | sed -E 's/^v//; s/[~_](alpha|beta|pre|rc)([0-9]+)$/-\1.\2/')
if ! printf '%s' "$UPSTREAM" | grep -Eq '^[0-9]+(\.[0-9]+)*(-(alpha|beta|pre|rc)\.[0-9]+)?$'; then
  echo "openwrt-package: version '$VERSION' is not a release version (1.2.3 or 1.2.3-rc.4)" >&2
  exit 1
fi
# The package version: a pre-release has to sort below its final release in
# each package manager. opkg: 1.0.0~rc2 < 1.0.0 (1.0.0_rc2 would sort above it).
# apk-tools: 1.0.0_rc2 < 1.0.0, and it rejects "~" and "-rc.2" outright.
case "$EXT" in
  ipk) sep='~' ;;
  apk) sep='_' ;;
esac
VERSION=$(printf '%s' "$UPSTREAM" | sed -E "s/-(alpha|beta|pre|rc)\.([0-9]+)$/${sep}\1\2/")
PKG_RELEASE=$(sed -n 's/^PKG_RELEASE:=//p' "openwrt/$PKG/Makefile")

OUT=out/openwrt
WORK="$OUT/work/$ARCH-$RELEASE"
DL="${OPENWRT_DL:-$OUT/dl}"
IMAGE="${SDK_IMAGE:-docker.io/openwrt/sdk:$ARCH-$RELEASE}"
GO_BOOTSTRAP="${GO_BOOTSTRAP-$(go env GOROOT 2>/dev/null || true)}"

rm -rf "$WORK"
mkdir -p "$WORK/feed" "$WORK/bin" "$DL"
cp -a "openwrt/$PKG" "$WORK/feed/"
sed -i -e "s/^PERCH_VERSION:=.*/PERCH_VERSION:=$UPSTREAM/" \
  -e "s/^PKG_VERSION:=.*/PKG_VERSION:=$VERSION/" "$WORK/feed/$PKG/Makefile"

if [ -n "${SOURCE_REF:-}" ]; then
  tree="$SOURCE_REF"
  source="$SOURCE_REF ($(git rev-parse --short "$SOURCE_REF^{commit}"))"
else
  tree=$(git stash create 2>/dev/null || true)
  source="$(git rev-parse --short HEAD)${tree:+ + local changes}"
  tree="${tree:-HEAD}"
fi
git archive --format=tar.gz --prefix="$PKG-$UPSTREAM/" -o "$DL/$PKG-$UPSTREAM.tar.gz" "$tree"
echo "openwrt-package: $PKG $VERSION-r$PKG_RELEASE for $ARCH on OpenWrt $RELEASE (.$EXT), source $source"

extra=()
if [ -n "$GO_BOOTSTRAP" ] && [ -x "$GO_BOOTSTRAP/bin/go" ]; then
  # Same path inside the container, plus wherever src/ really lives:
  # distribution packages (Debian, Ubuntu) symlink it out of GOROOT.
  root=$(realpath "$GO_BOOTSTRAP")
  extra+=(-v "$root:$root:ro" -e GO_BOOTSTRAP="$root")
  # Go >= 1.24 can build any host Go these SDKs need directly, which spares
  # 25.12 its chain of bootstrap compilers (1.17, 1.20, 1.22, 1.24) too.
  minor=$("$root/bin/go" env GOVERSION | sed -nE 's/^go1\.([0-9]+).*/\1/p')
  [ "${minor:-0}" -ge 24 ] && extra+=(-e GO_BOOTSTRAP_DIRECT=1)
  src=$(realpath "$root/src")
  case "$src" in
    "$root"/*) ;;
    *) extra+=(-v "$(dirname "$src"):$(dirname "$src"):ro") ;;
  esac
fi
# The SDK runs as buildbot (uid 1000); let it write the mounts whoever we are.
chmod -R a+rwX "$WORK" "$DL"

docker run --rm \
  -v "$(realpath "$WORK/feed"):/feed:ro" \
  -v "$(realpath "$DL"):/builder/dl" \
  -v "$(realpath "$WORK/bin"):/builder/bin" \
  -e PKG="$PKG" -e JOBS="${JOBS:-$(nproc)}" \
  "${extra[@]}" \
  --entrypoint /bin/bash "$IMAGE" -c '
set -euo pipefail
cd /builder
# Only the feeds the package needs (base for libpcap and friends, packages for
# Go), the OpenWrt tree shallow (24.10 lists a full clone of it), fetched from
# the GitHub mirrors at the same refs. 24.10 writes "src-git-full base …",
# 25.12 "src-git --root=package base …".
grep -E "^src-git(-full)? (--root=[^ ]+ )?(base|packages) " feeds.conf.default \
  | sed -E -e "s/^src-git-full /src-git /" \
        -e "s#https://git.openwrt.org/openwrt/openwrt.git#https://github.com/openwrt/openwrt.git#" \
        -e "s#https://git.openwrt.org/feed/packages.git#https://github.com/openwrt/packages.git#" > feeds.conf
cat feeds.conf
echo "src-link perch /feed" >> feeds.conf
echo "== feeds ($(date +%T))"
./scripts/feeds update -a >bin/feeds.log 2>&1 || { tail -40 bin/feeds.log; exit 1; }
./scripts/feeds install -p perch "$PKG" >>bin/feeds.log 2>&1 || { tail -40 bin/feeds.log; exit 1; }
echo "== configure ($(date +%T))"
make defconfig >/dev/null
if [ -n "${GO_BOOTSTRAP:-}" ]; then
  echo "CONFIG_GOLANG_EXTERNAL_BOOTSTRAP_ROOT=\"$GO_BOOTSTRAP\"" >> .config
  [ -n "${GO_BOOTSTRAP_DIRECT:-}" ] && echo "# CONFIG_GOLANG_BUILD_BOOTSTRAP is not set" >> .config
  make defconfig >/dev/null
fi
echo "== build ($(date +%T))"
if ! make "package/$PKG/compile" -j"$JOBS" V=s >bin/build.log 2>&1; then
  grep -n -E "(error|Error|ERROR)[: ]" bin/build.log | grep -v -E "^[0-9]+:.*-Werror|Werror=" | tail -30
  tail -30 bin/build.log
  exit 1
fi
echo "== done ($(date +%T))"
find bin/packages -name "$PKG*" -type f
'

built=$(find "$WORK/bin/packages" -type f -name "$PKG*.$EXT" | head -1)
[ -n "$built" ] || { echo "openwrt-package: no .$EXT produced" >&2; exit 1; }
# Named with the upstream version, so one $V builds both the tag and the file
# name in a download URL (and GitHub keeps "~" out of asset names).
dest="$OUT/${PKG}_${UPSTREAM}-r${PKG_RELEASE}_${ARCH}.$EXT"
cp "$built" "$dest"
echo "openwrt-package: $dest ($(stat -c %s "$dest") bytes)"
