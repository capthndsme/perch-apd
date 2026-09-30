MODULE  := github.com/capthndsme/perch-apd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
GOFLAGS := -trimpath
# nethttpomithttp2 leaves net/http's bundled HTTP/2 out: the daemon speaks
# HTTP/1.1 only (the kit's link.NewHTTPClient; a WebSocket cannot be
# upgraded over HTTP/2), and the unused HTTP/2 client costs a mipsle binary
# ~390 KB. A toolchain without the tag builds the same daemon, only larger.
TAGS    := nethttpomithttp2
DIST    := dist

export CGO_ENABLED := 0

.PHONY: build test vet release clean size

build:  ## native binary in out/
	go build $(GOFLAGS) -tags $(TAGS) -ldflags "$(LDFLAGS)" -o out/perch-apd ./cmd/perch-apd

test:
	go test ./...

vet:
	gofmt -l . | (! grep .) && go vet ./...

# Release assets: one static binary per architecture, checksums, installer.
#   amd64   x86_64
#   arm64   aarch64 (Filogic MT798x, IPQ807x, BCM27xx 64-bit)
#   armv7   ARMv7 with VFP (IPQ40xx, IPQ806x, mvebu, sunxi)
#   armv5   ARMv5/v6 and FPU-less ARMv7 (kirkwood, bcm53xx), software floats
#   mipsle  mipsel_24kc (MT7621, MT7628), software floats
#   mips    mips_24kc (ath79), software floats
release: clean
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -tags $(TAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/perch-apd-linux-amd64 ./cmd/perch-apd
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -tags $(TAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/perch-apd-linux-arm64 ./cmd/perch-apd
	GOOS=linux GOARCH=arm GOARM=7 go build $(GOFLAGS) -tags $(TAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/perch-apd-linux-armv7 ./cmd/perch-apd
	GOOS=linux GOARCH=arm GOARM=5 go build $(GOFLAGS) -tags $(TAGS) -ldflags "$(LDFLAGS) -X $(MODULE)/internal/version.goarm=v5" -o $(DIST)/perch-apd-linux-armv5 ./cmd/perch-apd
	GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build $(GOFLAGS) -tags $(TAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/perch-apd-linux-mipsle ./cmd/perch-apd
	GOOS=linux GOARCH=mips GOMIPS=softfloat go build $(GOFLAGS) -tags $(TAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/perch-apd-linux-mips ./cmd/perch-apd
	# install.sh installs the release it ships with: a version like 1.2.3 or
	# 1.2.3-rc.1 is stamped in, anything else (a CI or dev build) keeps latest.
	if printf '%s' '$(VERSION)' | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$$'; then \
		sed 's/^PERCH_APD_RELEASE=latest$$/PERCH_APD_RELEASE=v$(VERSION)/' scripts/install.sh > $(DIST)/install.sh; \
		grep -q '^PERCH_APD_RELEASE=v$(VERSION)$$' $(DIST)/install.sh; \
	else \
		cp scripts/install.sh $(DIST)/install.sh; \
	fi
	cd $(DIST) && sha256sum perch-apd-linux-* install.sh > checksums.txt
	@ls -l $(DIST)

# Size on the APs with the least flash: the mipsle release build.
# - The Wi-Fi config plane: the build with and without it (-tags noplane),
#   in bytes the binary loads (text, rodata, pclntab, data: `size -A` from
#   binutils; the file itself grows in 64 KiB pages). Fails over
#   PLANE_BUDGET (the kit's openwrt/plane and internal/wifiplane: ~580 KB
#   with go1.26, ~553 KB with go1.22).
# - The whole binary, gzip -9: what a jffs2 overlay stores, give or take
#   (LZMA per 4 KiB page ~1.07-1.13 x gzip). A self-update keeps the old
#   binary while it writes the new one, so on an 8 MB-flash AP (Archer AX23
#   class: ~4.1 MB free with perch-apd installed) the new binary must fit in
#   (free - 0.5 MB reserve) / 1.2 = 3080 KiB of gzip. Fails over
#   GZIP_BUDGET. Depends on the Go release: CI checks it with the release
#   toolchain.
PLANE_BUDGET ?= 600000
GZIP_BUDGET  ?= 3153920
SIZE_ENV     := GOOS=linux GOARCH=mipsle GOMIPS=softfloat
size:
	@mkdir -p out
	$(SIZE_ENV) go build $(GOFLAGS) -tags $(TAGS) -ldflags "$(LDFLAGS)" -o out/size-plane ./cmd/perch-apd
	$(SIZE_ENV) go build $(GOFLAGS) -tags $(TAGS),noplane -ldflags "$(LDFLAGS)" -o out/size-noplane ./cmd/perch-apd
	@loaded() { size -A "$$1" | awk 'NR > 2 && $$3 > 0 && $$1 !~ /bss/ { s += $$2 } END { print s }'; }; \
	with=$$(loaded out/size-plane); without=$$(loaded out/size-noplane); delta=$$((with - without)); \
	file=$$(wc -c < out/size-plane); gz=$$(gzip -9c out/size-plane | wc -c); \
	echo "perch-apd mipsle: $$file bytes, $$gz gzip -9 (budget $(GZIP_BUDGET)); the Wi-Fi config plane loads $$delta bytes (budget $(PLANE_BUDGET))"; \
	fail=0; \
	[ $$delta -le $(PLANE_BUDGET) ] || { echo "the Wi-Fi config plane is over its size budget"; fail=1; }; \
	[ $$gz -le $(GZIP_BUDGET) ] || { echo "perch-apd is over its gzip budget: it would not fit beside the old binary on an 8 MB-flash AP"; fail=1; }; \
	exit $$fail

clean:
	rm -rf $(DIST) out
