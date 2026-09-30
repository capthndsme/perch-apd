#!/usr/bin/env bash
# A local (unpublished) perch-apd release for your own controller: built from a
# commit, signed after scripts/sign-release.sh rebuilt it and found the same
# bytes, uploaded to the controller (agent-updates README section 6, channel
# "local"). Devices on the channel "local" are then offered it in
# Settings → Updates.
#
#   scripts/local-release.sh [options] <version>
#
# 1. Clean clones of this repository at --commit (default HEAD; uncommitted
#    changes are not part of a release) and, with an unpublished kit, of
#    perch-agentkit at its HEAD; nothing is built in this checkout.
# 2. The six release binaries (make release: CGO_ENABLED=0, -trimpath, fixed
#    ldflags, VERSION=<version> exactly, which is how the controller recognises
#    the new process) and the files bundle (make files) into
#    out/local-release/<version>/.
# 3. perch-manifest.json, channel local, the fields of release.env.
# 4. scripts/sign-release.sh --dir …: rebuilds everything from fresh clones,
#    compares, and signs after a yes (signify-openbsd asks the passphrase).
# 5. With --controller, the signed release is uploaded (POST the manifest, PUT
#    each file) with an API token: PERCH_API_TOKEN, --token-file, or asked
#    (hidden). A controller only stores what the key it trusts signed.
#
# Options:
#   --commit REF        what to release (default HEAD)
#   --kit DIR           perch-agentkit checkout to build against (default ../perch-agentkit
#                       when it exists); --published-kit builds against go.mod's kit version
#   --key FILE          secret key for sign-release.sh (default: asked)
#   --controller URL    upload to this controller (https://perch.example.com)
#   --token-file FILE   API token (node ace dev:token on the controller)
#   --insecure          do not verify the controller's TLS certificate
#   --yes               passed to sign-release.sh
#
# OpenWrt packages are not built here: a local release updates devices by
# binary swap (the live APs and hand-installed gateways are "swapped"/"unowned").
set -euo pipefail

PRODUCT=perch-apd
REPO=$(cd "$(dirname "$0")/.." && pwd)

die() { echo "local-release: $*" >&2; exit 1; }
say() { echo "== $*" >&2; }

COMMIT=HEAD KIT='' PUBLISHED_KIT=0 KEY='' CONTROLLER='' TOKEN_FILE='' INSECURE=0 YES=0 VERSION=''
while [ $# -gt 0 ]; do
  case "$1" in
    --commit) COMMIT=$2; shift ;;
    --kit) KIT=$(cd "$2" && pwd); shift ;;
    --published-kit) PUBLISHED_KIT=1 ;;
    --key) KEY=$2; shift ;;
    --controller) CONTROLLER=${2%/}; shift ;;
    --token-file) TOKEN_FILE=$2; shift ;;
    --insecure) INSECURE=1 ;;
    --yes) YES=1 ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
    -*) die "unknown option $1" ;;
    *) [ -z "$VERSION" ] || die "one version only"; VERSION=${1#v} ;;
  esac
  shift
done
[ -n "$VERSION" ] || die "usage: scripts/local-release.sh [options] <version>"
printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' \
  || die "$VERSION is not a release version (1.2.0, 1.1.0-pre.5): the daemon must report exactly it"
command -v go >/dev/null || die "go is not installed"

FULL=$(git -C "$REPO" rev-parse --verify "$COMMIT^{commit}")
[ -z "$(git -C "$REPO" status --porcelain --untracked-files=no)" ] \
  || echo "local-release: note: uncommitted changes are not part of the release (building $FULL)" >&2
if [ "$PUBLISHED_KIT" = 0 ] && [ -z "$KIT" ] && [ -f "$REPO/../perch-agentkit/go.mod" ]; then
  KIT=$(cd "$REPO/../perch-agentkit" && pwd)
fi
KIT_COMMIT=
if [ -n "$KIT" ]; then
  [ -z "$(git -C "$KIT" status --porcelain --untracked-files=no)" ] || die "$KIT has uncommitted changes: commit them first (the release records the kit commit)"
  KIT_COMMIT=$(git -C "$KIT" rev-parse HEAD)
fi

OUT="$REPO/out/local-release/$VERSION"
WORK=$(mkdir -p "$REPO/out" && mktemp -d "$REPO/out/local-work.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
rm -rf "$OUT" && mkdir -p "$OUT"

say "building $PRODUCT $VERSION from $FULL${KIT_COMMIT:+ with perch-agentkit $KIT_COMMIT}"
git clone -q --no-checkout "$REPO" "$WORK/src"
git -C "$WORK/src" -c advice.detachedHead=false checkout -q "$FULL"
GOWORK_FILE=off
if [ -n "$KIT" ]; then
  git clone -q --no-checkout "$KIT" "$WORK/perch-agentkit"
  git -C "$WORK/perch-agentkit" -c advice.detachedHead=false checkout -q "$KIT_COMMIT"
  kitv=$(sed -n 's|^[[:space:]]*github.com/capthndsme/perch-agentkit \(v[^ ]*\).*|\1|p' "$WORK/src/go.mod" | head -n1)
  gov=$(sed -n 's/^go //p' "$WORK/src/go.mod" "$WORK/perch-agentkit/go.mod" | sort -V | tail -n1)
  printf 'go %s\n\nuse (\n\t./src\n\t./perch-agentkit\n)\n\nreplace github.com/capthndsme/perch-agentkit %s => ./perch-agentkit\n' "$gov" "$kitv" > "$WORK/go.work"
  GOWORK_FILE="$WORK/go.work"
fi
build_env() {
  env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" GOWORK="$GOWORK_FILE" \
    ${GOPATH:+GOPATH="$GOPATH"} ${GOMODCACHE:+GOMODCACHE="$GOMODCACHE"} ${GOCACHE:+GOCACHE="$GOCACHE"} \
    ${GOPROXY:+GOPROXY="$GOPROXY"} ${GOSUMDB:+GOSUMDB="$GOSUMDB"} "$@"
}

PR="$WORK/bin/perch-release"
(cd "$REPO" && go build -o "$PR" github.com/capthndsme/perch-agentkit/cmd/perch-release)
build_env make -C "$WORK/src" release VERSION="$VERSION" >"$WORK/build.log" 2>&1 || { tail -20 "$WORK/build.log" >&2; die "build failed"; }
build_env make -C "$WORK/src" files PERCH_RELEASE="$PR" >>"$WORK/build.log" 2>&1 || { tail -20 "$WORK/build.log" >&2; die "bundle failed"; }
cp "$WORK/src/dist/"perch-apd-linux-* "$WORK/src/dist/perch-apd-files.tar.gz" "$OUT/"

# shellcheck disable=SC1091
. "$WORK/src/release.env"
"$PR" manifest -product "$PRODUCT" -version "$VERSION" -channel local -commit "$FULL" \
  -min-version "${MIN_VERSION:-}" -min-from-version "${MIN_FROM_VERSION:-}" \
  -min-controller-version "${MIN_CONTROLLER_VERSION:-}" -sdk-env "$WORK/src/openwrt/sdk.env" \
  -files-spec "$WORK/src/openwrt/perch-apd/files.json" -o "$OUT/perch-manifest.json" "$OUT"

sign=("$REPO/scripts/sign-release.sh" --dir "$OUT")
[ -n "$KIT" ] && sign+=(--kit "$KIT" --kit-commit "$KIT_COMMIT")
[ -n "$KEY" ] && sign+=(--key "$KEY")
[ "$YES" = 1 ] && sign+=(--yes)
"${sign[@]}" "$VERSION"

if [ -n "$CONTROLLER" ]; then
  if [ -z "${PERCH_API_TOKEN:-}" ]; then
    if [ -n "$TOKEN_FILE" ]; then
      PERCH_API_TOKEN=$(tr -d '\n' <"$TOKEN_FILE")
    else
      read -r -s -p "API token for $CONTROLLER: " PERCH_API_TOKEN </dev/tty; echo >&2
    fi
  fi
  up=(-controller "$CONTROLLER" -dir "$OUT")
  [ "$INSECURE" = 1 ] && up+=(-insecure)
  PERCH_API_TOKEN="$PERCH_API_TOKEN" "$PR" upload "${up[@]}"
  say "uploaded; Settings → Updates → Releases lists $PRODUCT $VERSION (channel local)"
fi
say "release files: $OUT"
