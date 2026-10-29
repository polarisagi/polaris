.PHONY: build build-backend build-ui rust-build generate-manifest test clean

GO := go
CARGO := cargo
CARGO_TARGET_DIR := rust/substrate/target/$(if $(CARGO_TARGET),$(CARGO_TARGET)/,)release
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.CommitHash=$(COMMIT) -X main.BuildDate=$(DATE)

build: generate-manifest rust-build build-ui
	@mkdir -p bin/lib
	@cp $(CARGO_TARGET_DIR)/libsubstrate.dylib bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/libsubstrate.so bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/substrate.dll bin/lib/ 2>/dev/null || true
	$(GO) build -ldflags="$(LDFLAGS)" -o bin/polaris ./cmd/polaris

build-backend: generate-manifest rust-build
	@mkdir -p bin/lib
	@cp $(CARGO_TARGET_DIR)/libsubstrate.dylib bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/libsubstrate.so bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/substrate.dll bin/lib/ 2>/dev/null || true
	$(GO) build -ldflags="$(LDFLAGS)" -o bin/polaris ./cmd/polaris

build-ui:
	cd web && npm ci && npm run build

rust-build:
	CFLAGS= LDFLAGS= $(CARGO) build --release $(if $(CARGO_TARGET),--target $(CARGO_TARGET),) --manifest-path rust/substrate/Cargo.toml

generate-manifest:
	GOOS= GOARCH= $(GO) run tools/generate_manifest.go

test:
	$(GO) test -skip '^(TestFSM_Spec|TestSpec.*)$$' ./internal/... ./cmd/... ./pkg/...
	$(CARGO) test --manifest-path rust/substrate/Cargo.toml

clean:
	rm -rf bin web/dist rust/substrate/target
