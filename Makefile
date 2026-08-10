VERSION ?= dev
BINARY  := docker-machine-driver-pve
PKG     := github.com/lore09/pve-rancher-driver
TARGETS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

GOFLAGS ?= -mod=mod

.PHONY: all build vet test fmt clean crossclean dist install-deps test-workflows

all: build

build:
	go build $(GOFLAGS) -o $(BINARY) ./cmd/$(BINARY)

vet:
	go vet $(GOFLAGS) ./...

test:
	go test $(GOFLAGS) ./...

fmt:
	gofmt -s -w .

dist: build
	mkdir -p dist
	@set -e; for target in $(TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "==> $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
		  go build $(GOFLAGS) \
		  -o dist/$(BINARY)-$$os-$$arch ./cmd/$(BINARY); \
	done
	@cd dist && for f in $(BINARY)-*; do sha256sum $$f >> checksums.txt; done

install-deps:
	go mod download

clean:
	rm -f $(BINARY)

crossclean:
	rm -rf dist

# Unit tests for the release-decision logic used by .github/workflows.
# Pure bash; no Go toolchain and no repository state required.
test-workflows:
	bash .github/scripts/detect-release.test.sh
	bash .github/scripts/govulncheck-gate.test.sh