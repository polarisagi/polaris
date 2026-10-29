.PHONY: build build-backend build-ui rust-build generate-manifest test clean

GO := go
CARGO := cargo
CARGO_TARGET_DIR := rust/substrate/target/$(if $(CARGO_TARGET),$(CARGO_TARGET)/,)release
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.CommitHash=$(COMMIT) -X main.BuildDate=$(DATE)

# CI 优化：SKIP_RUST_BUILD=1 时跳过 Rust 编译（release.yml 已通过预编译 artifact 获取）。
SKIP_RUST_BUILD ?=
_RUST_DEP := $(if $(SKIP_RUST_BUILD),,rust-build)

build: generate-manifest $(_RUST_DEP) build-ui
	@mkdir -p bin/lib
	@cp $(CARGO_TARGET_DIR)/libsubstrate.dylib bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/libsubstrate.so bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/substrate.dll bin/lib/ 2>/dev/null || true
	$(GO) build -ldflags="$(LDFLAGS)" -o bin/polaris ./cmd/polaris

build-backend: generate-manifest $(_RUST_DEP)
	@mkdir -p bin/lib
	@cp $(CARGO_TARGET_DIR)/libsubstrate.dylib bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/libsubstrate.so bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/substrate.dll bin/lib/ 2>/dev/null || true
	$(GO) build -ldflags="$(LDFLAGS)" -o bin/polaris ./cmd/polaris

build-ui:
	cd web && npm ci && npm run build
	@touch web/dist/.gitkeep

rust-build:
	CFLAGS= LDFLAGS= $(CARGO) build --release $(if $(CARGO_TARGET),--target $(CARGO_TARGET),) --manifest-path rust/substrate/Cargo.toml

generate-manifest:
	GOOS= GOARCH= $(GO) run tools/generate_manifest.go

test:
	$(GO) test -skip '^(TestFSM_Spec|TestSpec.*)$$' ./internal/... ./cmd/... ./pkg/...
	$(CARGO) test --manifest-path rust/substrate/Cargo.toml

clean:
	rm -rf bin web/dist rust/substrate/target
