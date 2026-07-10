# Steward build automation.
#
# `make build`  — host binary into bin/
# `make cross`  — all release targets into dist/ (linux+darwin, amd64+arm64)

BINARY    := steward
MAIN      := ./cmd/steward
BIN_DIR   := bin
DIST_DIR  := dist

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS   := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildDate=$(DATE)

# Proxmox VMs / LXC (linux) + Mac Minis / Mac Pros (darwin), Intel + Apple Silicon.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all build cross checksums test vet fmt lint tidy clean $(PLATFORMS)

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(MAIN)

cross: $(PLATFORMS)

$(PLATFORMS):
	CGO_ENABLED=0 GOOS=$(word 1,$(subst /, ,$@)) GOARCH=$(word 2,$(subst /, ,$@)) \
		go build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(DIST_DIR)/$(BINARY)-$(word 1,$(subst /, ,$@))-$(word 2,$(subst /, ,$@)) $(MAIN)

checksums: cross
	cd $(DIST_DIR) && shasum -a 256 $(BINARY)-* > SHA256SUMS

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint:
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || echo "golangci-lint not installed; skipping"

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)
