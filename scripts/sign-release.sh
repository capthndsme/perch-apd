#!/usr/bin/env bash
# Sign a perch-apd release, only after rebuilding it here and finding the same
# artefacts (agent-updates README D1, BUILD-PLAN WP-R). The owner runs this;
# the signing key never goes to CI or a controller.
#
#   scripts/sign-release.sh [options] <version>
#
# 1. The release files: the GitHub release v<version> (downloaded with gh into
#    out/sign/<version>/), or --dir DIR (a local build, scripts/local-release.sh).
# 2. The published files must be what their unsigned perch-manifest.json says
#    (perch-release verify: sizes, SHA-256, ELF architecture, package contents).
# 3. A clean clone of this repository at the manifest's commit is built again:
#    - the binaries with `make release` and the Go toolchain the published
#      binaries record (go version -m), GOWORK=off (or --kit for an unpublished
#      kit): each must be the same byte for byte;
#    - every OpenWrt package with scripts/openwrt-package.sh (Docker, the SDK
#      image of openwrt/sdk.env; 7-18 minutes each): the files it installs, their
#      modes and hashes, its maintainer scripts and dependencies must match
#      (archive timestamps and other metadata may differ);
#    - the files bundle with `make files`: the same members.
#    Any difference stops here with what differed. Nothing is signed.
# 4. After a yes, signify-openbsd signs the manifest. It asks for the key's
#    passphrase itself (on the terminal): this script never sees, stores or
#    passes it. The signature is checked against the public key.
# 5. With --upload, the .sig is attached to the GitHub release (gh).
#
# Options:
#   --dir DIR          release files already here (manifest + artefacts)
#   --key FILE         secret key (default: asked, ~/.config/perch/perch-release-1.sec)
#   --pub FILE         its public key (default: the .sec path with .pub)
#   --commit REF       rebuild this instead of the manifest's commit (must be the same commit)
#   --kit DIR          build against this perch-agentkit checkout (an unpublished kit,
#                      local releases) instead of the published module
#   --kit-commit REF   the kit commit the release was built with (default: DIR's HEAD)
#   --no-packages      do not rebuild the OpenWrt packages: they are left out of a new
#                      manifest (binaries and bundle only), which replaces the published one
#   --upload           attach the signature (and a replaced manifest) to the GitHub release
#   --yes              do not ask before signing (the passphrase is still asked)
#   --keep             keep the work directory (clones, rebuilds)
#
# Needs: git, go, gh (downloads, --upload), docker (packages), signify-openbsd
# (Debian/Ubuntu: apt install signify-openbsd; SIGNIFY=/path overrides).
set -euo pipefail

PRODUCT=perch-apd
GH_REPO=capthndsme/perch-apd
REPO=$(cd "$(dirname "$0")/.." && pwd)

die() { echo "sign-release: $*" >&2; exit 1; }
say() { echo "== $*" >&2; }
need() { for c in "$@"; do command -v "$c" >/dev/null 2>&1 || die "$c is not installed"; done; }

DIR='' KEY='' PUB='' COMMIT='' KIT='' KIT_COMMIT='' PACKAGES=1 UPLOAD=0 YES=0 KEEP=0 VERSION=''
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) DIR=$(cd "$2" && pwd); shift ;;
    --key) KEY=$2; shift ;;
    --pub) PUB=$2; shift ;;
    --commit) COMMIT=$2; shift ;;
    --kit) KIT=$(cd "$2" && pwd); shift ;;
    --kit-commit) KIT_COMMIT=$2; shift ;;
    --no-packages) PACKAGES=0 ;;
    --upload) UPLOAD=1 ;;
    --yes) YES=1 ;;
    --keep) KEEP=1 ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
    -*) die "unknown option $1" ;;
    *) [ -z "$VERSION" ] || die "one version only"; VERSION=${1#v} ;;
  esac
  shift
done
[ -n "$VERSION" ] || die "usage: scripts/sign-release.sh [options] <version>"
printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' || die "$VERSION is not a release version"
need git go
SIGNIFY=${SIGNIFY:-$(command -v signify-openbsd || command -v signify || true)}
[ -n "$SIGNIFY" ] || die "signify-openbsd is not installed (apt install signify-openbsd)"

WORK=$(mktemp -d "$REPO/out/sign-work.XXXXXX" 2>/dev/null || { mkdir -p "$REPO/out" && mktemp -d "$REPO/out/sign-work.XXXXXX"; })
cleanup() {
  if [ "$KEEP" = 1 ] || [ "${OK:-0}" != 1 ]; then echo "sign-release: work directory kept: $WORK" >&2; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

# ── 1. the release files ─────────────────────────────────────────────────────
SOURCE=local
if [ -z "$DIR" ]; then
  need gh
  SOURCE=github
  DIR="$REPO/out/sign/$VERSION"
  rm -rf "$DIR" && mkdir -p "$DIR"
  say "downloading the GitHub release v$VERSION of $GH_REPO"
  gh release download "v$VERSION" -R "$GH_REPO" -D "$DIR"
fi
MANIFEST="$DIR/perch-manifest.json"
[ -f "$MANIFEST" ] || die "no perch-manifest.json in $DIR (CI attaches it once the packages are built)"

# perch-release from the kit this checkout builds with (go.work's local kit, or
# the version go.mod pins, checked against go.sum).
PR="$WORK/bin/perch-release"
(cd "$REPO" && go build -o "$PR" github.com/capthndsme/perch-agentkit/cmd/perch-release) || die "cannot build perch-release"

field() { "$PR" info "$MANIFEST" | sed -n "s/^$1=//p"; }
[ "$(field product)" = "$PRODUCT" ] || die "the manifest is for $(field product), not $PRODUCT"
[ "$(field version)" = "$VERSION" ] || die "the manifest is for version $(field version), not $VERSION"
CHANNEL=$(field channel)
FLOOR=$(field minVersion) FROMV=$(field minFromVersion) CTLV=$(field minControllerVersion)
MCOMMIT=$(field commit)
[ -n "$MCOMMIT" ] || die "the manifest names no commit: nothing to rebuild from"
COMMIT=${COMMIT:-$MCOMMIT}
FULL=$(git -C "$REPO" rev-parse --verify --quiet "$COMMIT^{commit}") || die "commit $COMMIT is not in this repository (git fetch --tags)"
case "$FULL" in "$MCOMMIT"*) ;; *) die "--commit $COMMIT is $FULL, the manifest says $MCOMMIT" ;; esac
if [ "$SOURCE" = github ]; then
  TAGGED=$(git -C "$REPO" rev-parse --verify --quiet "v$VERSION^{commit}") || die "tag v$VERSION is not in this repository (git fetch --tags)"
  [ "$TAGGED" = "$FULL" ] || die "tag v$VERSION is $TAGGED, the manifest was built from $FULL"
fi

# ── 2. a clean clone at the commit (and the kit) ─────────────────────────────
say "cloning $FULL"
git clone -q --no-checkout "$REPO" "$WORK/src"
git -C "$WORK/src" -c advice.detachedHead=false checkout -q "$FULL"
GOWORK_FILE=off
if [ -n "$KIT" ]; then
  KIT_COMMIT=$(git -C "$KIT" rev-parse --verify "${KIT_COMMIT:-HEAD}^{commit}")
  say "building against perch-agentkit $KIT_COMMIT (local kit)"
  git clone -q --no-checkout "$KIT" "$WORK/perch-agentkit"
  git -C "$WORK/perch-agentkit" -c advice.detachedHead=false checkout -q "$KIT_COMMIT"
  kitv=$(sed -n 's|^[[:space:]]*github.com/capthndsme/perch-agentkit \(v[^ ]*\).*|\1|p' "$WORK/src/go.mod" | head -n1)
  gov=$(sed -n 's/^go //p' "$WORK/src/go.mod" "$WORK/perch-agentkit/go.mod" | sort -V | tail -n1)
  printf 'go %s\n\nuse (\n\t./src\n\t./perch-agentkit\n)\n\nreplace github.com/capthndsme/perch-agentkit %s => ./perch-agentkit\n' "$gov" "$kitv" > "$WORK/go.work"
  GOWORK_FILE="$WORK/go.work"
fi

say "checking the published files against the manifest"
"$PR" verify -unsigned -dir "$DIR" -sdk-env "$WORK/src/openwrt/sdk.env" "$MANIFEST"

# A build environment with nothing from this shell but what a build needs.
build_env() {
  env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" GOWORK="$GOWORK_FILE" \
    ${GOPATH:+GOPATH="$GOPATH"} ${GOMODCACHE:+GOMODCACHE="$GOMODCACHE"} ${GOCACHE:+GOCACHE="$GOCACHE"} \
    ${GOPROXY:+GOPROXY="$GOPROXY"} ${GOSUMDB:+GOSUMDB="$GOSUMDB"} ${DOCKER_HOST:+DOCKER_HOST="$DOCKER_HOST"} "$@"
}

REBUILT="$WORK/rebuilt"
mkdir -p "$REBUILT"
BINARIES=() PACKAGE_LINES=() HAVE_BUNDLE=0
while read -r kind file rest; do
  case "$kind" in
    binary) BINARIES+=("$file") ;;
    package) PACKAGE_LINES+=("$file $rest") ;;
    files) HAVE_BUNDLE=1 ;;
  esac
done < <("$PR" info "$MANIFEST" | sed -n 's/^artefact=//p')

# ── 3a. binaries: the toolchain the published binaries were built with ──────
if [ ${#BINARIES[@]} -gt 0 ]; then
  GOV=$(go version "$DIR/${BINARIES[0]}" | awk '{print $2}')
  printf '%s' "$GOV" | grep -Eq '^go[0-9]+\.[0-9]+(\.[0-9]+)?$' || die "${BINARIES[0]} records no Go release ($GOV)"
  for b in "${BINARIES[@]}"; do
    [ "$(go version "$DIR/$b" | awk '{print $2}')" = "$GOV" ] || die "$b was built with another Go than ${BINARIES[0]}"
  done
  say "rebuilding ${#BINARIES[@]} binaries with $GOV"
  build_env GOTOOLCHAIN="$GOV" make -C "$WORK/src" release VERSION="$VERSION" >"$WORK/build-binaries.log" 2>&1 \
    || { tail -20 "$WORK/build-binaries.log" >&2; die "the rebuild failed"; }
  for b in "${BINARIES[@]}"; do cp "$WORK/src/dist/$b" "$REBUILT/"; done
fi

# ── 3b. the files bundle ────────────────────────────────────────────────────
if [ "$HAVE_BUNDLE" = 1 ]; then
  say "rebuilding the files bundle"
  build_env make -C "$WORK/src" files PERCH_RELEASE="$PR" >"$WORK/build-files.log" 2>&1 \
    || { cat "$WORK/build-files.log" >&2; die "the bundle rebuild failed"; }
  cp "$WORK/src/dist/$PRODUCT-files.tar.gz" "$REBUILT/"
fi

# ── 3c. OpenWrt packages ────────────────────────────────────────────────────
SIGN_MANIFEST="$MANIFEST"
if [ ${#PACKAGE_LINES[@]} -gt 0 ] && [ "$PACKAGES" = 1 ]; then
  need docker
  # shellcheck disable=SC1091
  . "$WORK/src/openwrt/sdk.env"
  say "rebuilding ${#PACKAGE_LINES[@]} OpenWrt packages in the SDK images (7-18 minutes each)"
  for line in "${PACKAGE_LINES[@]}"; do
    read -r file pkgarch _manager series _pkg <<<"$line"
    rel=
    for r in $OPENWRT_RELEASES; do case "$r" in "$series".*) rel=$r ;; esac; done
    [ -n "$rel" ] || die "$file: openwrt/sdk.env at $FULL builds no OpenWrt $series"
    say "  $file: $pkgarch on OpenWrt $rel"
    (cd "$WORK/src" && build_env SOURCE_REF="$FULL" OPENWRT_DL="$REPO/out/openwrt/dl" JOBS="${JOBS:-$(nproc)}" \
      scripts/openwrt-package.sh "$pkgarch" "$rel" "v$VERSION") >"$WORK/build-$file.log" 2>&1 \
      || { tail -20 "$WORK/build-$file.log" >&2; die "$file: the package rebuild failed"; }
    cp "$WORK/src/out/openwrt/$file" "$REBUILT/" || die "$file: the rebuild made another file name"
    rm -rf "$WORK/src/out/openwrt/work"
  done
elif [ ${#PACKAGE_LINES[@]} -gt 0 ]; then
  # Without the packages: a new manifest of what was rebuilt, which replaces
  # the published one (package installs then update by binary swap).
  say "--no-packages: signing a new manifest without the ${#PACKAGE_LINES[@]} packages"
  mkdir -p "$WORK/nopkg"
  for b in "${BINARIES[@]}"; do cp "$DIR/$b" "$WORK/nopkg/"; done
  [ "$HAVE_BUNDLE" = 0 ] || cp "$DIR/$PRODUCT-files.tar.gz" "$WORK/nopkg/"
  "$PR" manifest -product "$PRODUCT" -version "$VERSION" -channel "$CHANNEL" -commit "$MCOMMIT" \
    -released-at "$(field releasedAt)" -notes-url "$(field notesUrl)" -min-version "$(field minVersion)" \
    -min-from-version "$(field minFromVersion)" -min-controller-version "$(field minControllerVersion)" \
    -sdk-env "$WORK/src/openwrt/sdk.env" -files-spec "$WORK/src/openwrt/perch-apd/files.json" \
    -o "$WORK/nopkg/perch-manifest.json" "$WORK/nopkg"
  cp "$MANIFEST" "$DIR/perch-manifest.published.json"
  cp "$WORK/nopkg/perch-manifest.json" "$MANIFEST"
fi

# ── 4. compare, then sign ───────────────────────────────────────────────────
say "comparing the published artefacts with the rebuild"
if ! "$PR" compare -manifest "$SIGN_MANIFEST" -published "$DIR" -rebuilt "$REBUILT" -sdk-env "$WORK/src/openwrt/sdk.env" -json "$WORK/compare.json"; then
  echo >&2
  echo "sign-release: REFUSED. What was published is not what this commit builds; nothing was signed." >&2
  echo "sign-release: the findings are above (and in $WORK/compare.json)." >&2
  exit 1
fi

if [ -z "$KEY" ]; then
  KEY="$HOME/.config/perch/perch-release-1.sec"
  if [ -t 0 ] || [ -r /dev/tty ]; then
    read -r -p "Secret key [$KEY]: " answer </dev/tty || true
    KEY=${answer:-$KEY}
  fi
fi
[ -f "$KEY" ] || die "no secret key at $KEY (scripts/release-keygen.sh in perch-agentkit makes one)"
PUB=${PUB:-${KEY%.sec}.pub}
[ -f "$PUB" ] || die "no public key at $PUB"

echo
echo "  $PRODUCT $VERSION ($CHANNEL), commit $FULL"
echo "  floor ${FLOOR:-none}, installs over ${FROMV:-any version}, controller ${CTLV:-any}"
echo "  $("$PR" info "$MANIFEST" | grep -c '^artefact=') artefacts, every one matched the rebuild"
echo "  key $KEY"
echo
if [ "$YES" != 1 ]; then
  read -r -p "Sign this manifest? [y/N] " answer </dev/tty
  case "$answer" in y|Y|yes) ;; *) die "not signed" ;; esac
fi
# signify-openbsd reads the passphrase from the terminal itself.
"$SIGNIFY" -S -s "$KEY" -m "$MANIFEST" -x "$MANIFEST.sig" || die "signing failed (wrong passphrase?)"
"$PR" verify -key "$PUB" -dir "$DIR" "$MANIFEST"

# ── 5. publish the signature ────────────────────────────────────────────────
if [ "$UPLOAD" = 1 ]; then
  [ "$SOURCE" = github ] || die "--upload is for GitHub releases; local builds go to a controller (scripts/local-release.sh)"
  files=("$MANIFEST.sig")
  [ -f "$DIR/perch-manifest.published.json" ] && files+=("$MANIFEST")
  gh release upload "v$VERSION" -R "$GH_REPO" --clobber "${files[@]}"
  say "attached to https://github.com/$GH_REPO/releases/tag/v$VERSION: ${files[*]##*/}"
fi
OK=1
say "signed: $MANIFEST.sig"
