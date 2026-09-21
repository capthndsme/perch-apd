MODULE  := github.com/capthndsme/perch-apd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
GOFLAGS := -trimpath
DIST    := dist

export CGO_ENABLED := 0

.PHONY: build test vet release clean

build:  ## native binary in out/
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o out/perch-apd ./cmd/perch-apd

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
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/perch-apd-linux-amd64 ./cmd/perch-apd
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/perch-apd-linux-arm64 ./cmd/perch-apd
	GOOS=linux GOARCH=arm GOARM=7 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/perch-apd-linux-armv7 ./cmd/perch-apd
	GOOS=linux GOARCH=arm GOARM=5 go build $(GOFLAGS) -ldflags "$(LDFLAGS) -X $(MODULE)/internal/version.goarm=v5" -o $(DIST)/perch-apd-linux-armv5 ./cmd/perch-apd
	GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/perch-apd-linux-mipsle ./cmd/perch-apd
	GOOS=linux GOARCH=mips GOMIPS=softfloat go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/perch-apd-linux-mips ./cmd/perch-apd
	cp scripts/install.sh $(DIST)/install.sh
	cd $(DIST) && sha256sum perch-apd-linux-* install.sh > checksums.txt
	@ls -l $(DIST)

clean:
	rm -rf $(DIST) out
