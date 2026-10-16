BINARY  := spacehound
PKG     := ./cmd/spacehound
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
DIST    := dist
GO_TEST_FLAGS ?=
export CGO_ENABLED := 1

.PHONY: all build test vet lint fmt fmt-check check demo demo-json snapshot clean

all: check

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

test:
	go test -race $(GO_TEST_FLAGS) ./...

vet:
	go vet ./...

lint: fmt-check vet

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: fmt-check vet test build

# Safe executable checks: both run only against a generated temp fixture.
demo-json: build
	./$(BINARY) --demo --json

demo: build
	./$(BINARY) --demo

# Local release snapshot: darwin archives plus SHA256SUMS in dist/.
snapshot:
	rm -rf $(DIST) && mkdir -p $(DIST)
	set -eu; for arch in amd64 arm64; do \
		case $$arch in amd64) clang_arch=x86_64;; arm64) clang_arch=arm64;; esac; \
		dir=$(DIST)/$(BINARY)_$(VERSION)_darwin_$$arch; \
		mkdir -p "$$dir"; \
		CC="clang -arch $$clang_arch" GOOS=darwin GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o "$$dir/$(BINARY)" $(PKG); \
		cp README.md SAFETY.md LICENSE "$$dir/"; \
		tar -C $(DIST) -czf "$$dir.tar.gz" $$(basename "$$dir"); \
		rm -rf "$$dir"; \
	done
	cd $(DIST) && shasum -a 256 *.tar.gz > SHA256SUMS && cat SHA256SUMS

clean:
	rm -rf $(BINARY) $(DIST)
