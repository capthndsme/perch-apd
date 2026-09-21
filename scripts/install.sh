#!/bin/sh
# Perch AP Daemon (perch-apd) one-line installer for OpenWrt:
#
#   wget -qO- https://github.com/capthndsme/perch-apd/releases/latest/download/install.sh \
#     | sh -s -- --controller https://perch.example.com --token mlap_...
#
# Picks the binary for this router's architecture, checks it against the
# release's checksums.txt, then runs `perch-apd --install` with the
# arguments given here (without them it asks for the controller and token).
# PERCH_APD_BASE_URL overrides where the binaries come from.
set -eu

BASE_URL="${PERCH_APD_BASE_URL:-https://github.com/capthndsme/perch-apd/releases/latest/download}"

die() { echo "install.sh: $*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run as root"

arch=""
if [ -r /etc/openwrt_release ]; then
	. /etc/openwrt_release
	case "${DISTRIB_ARCH:-}" in
		x86_64) arch=amd64 ;;
		aarch64*) arch=arm64 ;;
		# ARMv7 with a VFP/NEON unit: hardware floats. Everything else ARM
		# (ARMv5/v6, FPU-less Cortex-A9 like bcm53xx): the soft-float build.
		arm_cortex-a*vfp*|arm_cortex-a*neon*) arch=armv7 ;;
		arm_*) arch=armv5 ;;
		mipsel*) arch=mipsle ;;
		mips_*|mips64*) arch=mips ;;
	esac
fi
if [ -z "$arch" ]; then
	case "$(uname -m)" in
		x86_64) arch=amd64 ;;
		aarch64|arm64) arch=arm64 ;;
		armv7*|armv8l) arch=armv7 ;;
		armv5*|armv6*) arch=armv5 ;;
		mips)
			# uname says mips for both byte orders: ask the ELF header of busybox.
			if [ "$(dd if=/bin/busybox bs=1 skip=5 count=1 2>/dev/null | hexdump -e '1/1 "%d"' 2>/dev/null)" = 1 ]; then
				arch=mipsle
			else
				arch=mips
			fi
			;;
	esac
fi
[ -n "$arch" ] || die "unsupported architecture: $(uname -m) ${DISTRIB_ARCH:-}"

fetch() {
	if command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	elif command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	else
		die "neither wget nor curl found"
	fi
}

file="perch-apd-linux-$arch"
tmp="/tmp/perch-apd.$$"
trap 'rm -f "$tmp" "$tmp.sums"' EXIT INT TERM

echo "Downloading $file ..."
fetch "$BASE_URL/$file" "$tmp" || die "download failed: $BASE_URL/$file"
if fetch "$BASE_URL/checksums.txt" "$tmp.sums" 2>/dev/null; then
	want="$(grep " $file\$" "$tmp.sums" | cut -d' ' -f1)"
	got="$(sha256sum "$tmp" | cut -d' ' -f1)"
	[ -n "$want" ] || die "$file is not listed in checksums.txt"
	[ "$want" = "$got" ] || die "checksum mismatch for $file"
	echo "Checksum OK."
else
	echo "Warning: no checksums.txt next to the binary; skipping verification." >&2
fi
chmod +x "$tmp"

# Keep a TTY for the prompts even though our own stdin is the pipe.
if [ -t 1 ] && [ -r /dev/tty ]; then
	"$tmp" --install "$@" < /dev/tty
else
	"$tmp" --install "$@"
fi
