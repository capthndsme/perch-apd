#!/usr/bin/env bash
# Build this repository's OpenWrt package with the official SDK container
# (docker.io/openwrt/sdk) for one package architecture and one OpenWrt release.
#
#   scripts/openwrt-package.sh <arch> <openwrt-release> [version]
#
#   arch             package architecture, e.g. mipsel_24kc (MT7621) or x86_64;
#                    openwrt/sdk.env lists what releases are built for
#   openwrt-release  24.10.x builds an .ipk (opkg), 25.12.x an .apk (apk-tools)
#   version          package version; default: the tag at HEAD or SOURCE_REF
#                    (v1.2.3 → 1.2.3), else PKG_VERSION from the package Makefile
#
# The source is this checkout (git archive of HEAD, plus uncommitted changes to
# tracked files) or SOURCE_REF, placed in the SDK's download directory, so the
# package is built from exactly that tree and nothing is fetched from GitHub.
# The packaging (openwrt/, this script) always comes from the checkout, which
# lets a newer checkout package an older tag.
#
# Result: out/openwrt/<package>_<version>-r<release>_<arch>.{ipk,apk}; the
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
    VERSION=$(sed -n 's/^PKG_VERSION:=//p' "openwrt/$PKG/Makefile")
  fi
fi
# apk-tools (OpenWrt 25.12) accepts 1.2.3 and 1.2.3_rc1 but not 1.2.3-rc1, and
# both package managers need the same string, so a pre-release tag is mapped.
VERSION=$(printf '%s' "$VERSION" | sed -E 's/^v//; s/-(alpha|beta|pre|rc|p)\.?([0-9]*)$/_\1\2/')
if ! printf '%s' "$VERSION" | grep -Eq '^[0-9]+(\.[0-9]+)*(_(alpha|beta|pre|rc|p)[0-9]*)?$'; then
  echo "openwrt-package: version '$VERSION' is not a valid OpenWrt package version" >&2
  exit 1
fi
PKG_RELEASE=$(sed -n 's/^PKG_RELEASE:=//p' "openwrt/$PKG/Makefile")

OUT=out/openwrt
WORK="$OUT/work/$ARCH-$RELEASE"
DL="${OPENWRT_DL:-$OUT/dl}"
IMAGE="${SDK_IMAGE:-docker.io/openwrt/sdk:$ARCH-$RELEASE}"
GO_BOOTSTRAP="${GO_BOOTSTRAP-$(go env GOROOT 2>/dev/null || true)}"

rm -rf "$WORK"
mkdir -p "$WORK/feed" "$WORK/bin" "$DL"
cp -a "openwrt/$PKG" "$WORK/feed/"
sed -i "s/^PKG_VERSION:=.*/PKG_VERSION:=$VERSION/" "$WORK/feed/$PKG/Makefile"

if [ -n "${SOURCE_REF:-}" ]; then
  tree="$SOURCE_REF"
  source="$SOURCE_REF ($(git rev-parse --short "$SOURCE_REF^{commit}"))"
else
  tree=$(git stash create 2>/dev/null || true)
  source="$(git rev-parse --short HEAD)${tree:+ + local changes}"
  tree="${tree:-HEAD}"
fi
git archive --format=tar.gz --prefix="$PKG-$VERSION/" -o "$DL/$PKG-$VERSION.tar.gz" "$tree"
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
dest="$OUT/${PKG}_${VERSION}-r${PKG_RELEASE}_${ARCH}.$EXT"
cp "$built" "$dest"
echo "openwrt-package: $dest ($(stat -c %s "$dest") bytes)"
