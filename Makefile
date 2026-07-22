.PHONY: build run test lint clean rust-build rust-test build-ui dev-ui docs-sync docs-check docs-lint gen-threshold-examples generate-manifest build-backend build-tier1 test-race rust-lint rust-audit fuzz-taint rust-deny deadcode check-all

GO := go
CARGO := cargo
BINARY := polaris
WEBUI_DIR := web

# VERSION 优先读环境变量（CI 通过 VERSION=github.ref_name 注入精确 tag）
# 本地开发降级到 git describe；都失败则用 "dev"
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE     := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS  := -s -w -X main.Version=$(VERSION) -X main.CommitHash=$(COMMIT) -X main.BuildDate=$(DATE)

CARGO_TARGET ?=
CARGO_TARGET_DIR := rust/substrate/target/$(if $(CARGO_TARGET),$(CARGO_TARGET)/,)release

# CI 优化：SKIP_RUST_BUILD=1 时跳过 Rust 编译（已通过 artifact 获取预编译 .so）
SKIP_RUST_BUILD ?=
_RUST_DEP := $(if $(SKIP_RUST_BUILD),,rust-build)

build: generate-manifest $(_RUST_DEP) build-ui
	@mkdir -p bin/lib
	@cp $(CARGO_TARGET_DIR)/libsubstrate.dylib bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/libsubstrate.so bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/substrate.dll bin/lib/ 2>/dev/null || true
	$(GO) build -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/polaris

build-backend: generate-manifest $(_RUST_DEP)
	@mkdir -p bin/lib
	@cp $(CARGO_TARGET_DIR)/libsubstrate.dylib bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/libsubstrate.so bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/substrate.dll bin/lib/ 2>/dev/null || true
	$(GO) build -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/polaris

build-tier1: generate-manifest rust-build-tier1 build-ui
	@mkdir -p bin/lib
	@cp $(CARGO_TARGET_DIR)/libsubstrate.dylib bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/libsubstrate.so bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/substrate.dll bin/lib/ 2>/dev/null || true
	$(GO) build -tags tier1 -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/polaris

build-release: generate-manifest rust-build
	@mkdir -p bin/lib
	@cp $(CARGO_TARGET_DIR)/libsubstrate.dylib bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/libsubstrate.so bin/lib/ 2>/dev/null || true
	@cp $(CARGO_TARGET_DIR)/substrate.dll bin/lib/ 2>/dev/null || true
	$(GO) build -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/polaris
	openssl dgst -sha256 bin/$(BINARY) | awk '{print $$NF}' > bin/$(BINARY).sha256
	@echo "==> 封印文件: bin/$(BINARY).sha256"


build-ui:
	@cd $(WEBUI_DIR) && npm install --silent && npm run build

dev-ui:
	@cd $(WEBUI_DIR) && npm install --silent && npm run dev

run:
	$(GO) run ./cmd/polaris

test:
	$(GO) test ./internal/...

lint:
	golangci-lint run ./...
	env GOOS=wasip1 GOARCH=wasm golangci-lint run ./internal/extension/skill/sdk/...

clean:
	rm -rf bin/ bin/lib
	$(CARGO) clean --manifest-path rust/substrate/Cargo.toml

# 重写 docs/arch/*.md 头部 §跳读 行号 (从实际 ## headers 同步)
docs-sync:
	env GOOS= GOARCH= $(GO) run tools/sync_doc_toc.go

# CI 用: 校验 §跳读 与实际 headers 一致, drift 时退出非零
docs-check:
	env GOOS= GOARCH= $(GO) run tools/sync_doc_toc.go -check

# 文档级 Go 代码块禁令 (#9): M_X 中不得出现 ```go / type X struct|interface / func 签名块.
# 接口签名权威源在 internal/protocol/, 文档只允许字段名清单 + 单行语义 + Schema Anchor.
docs-lint:
	@bad=0 ; \
	if grep -rnE '^```(go|rust)' docs/arch/M*.md ; then echo "FAIL: 禁止 \`\`\`go/\`\`\`rust 代码块" ; bad=1 ; fi ; \
	if grep -rnE '^\s*type\s+\w+\s+(struct|interface)\s*\{' docs/arch/M*.md ; then echo "FAIL: 禁止裸 type struct/interface 定义" ; bad=1 ; fi ; \
	if grep -rnE '^\s*func\s+(\([^)]+\)\s+)?\w+\([^)]*\)' docs/arch/M*.md ; then echo "FAIL: 禁止完整 func 签名" ; bad=1 ; fi ; \
	if [ $$bad -ne 0 ]; then exit 1; fi ; \
	echo "docs-lint ok"

rust-build:
	CFLAGS= LDFLAGS= $(CARGO) build --release $(if $(CARGO_TARGET),--target $(CARGO_TARGET),) --manifest-path rust/substrate/Cargo.toml

rust-build-tier1:
	CFLAGS= LDFLAGS= $(CARGO) build --release $(if $(CARGO_TARGET),--target $(CARGO_TARGET),) --features tier1 --manifest-path rust/substrate/Cargo.toml

rust-test:
	CFLAGS= LDFLAGS= $(CARGO) test --manifest-path rust/substrate/Cargo.toml

fmt:
	$(GO) fmt ./...
	$(CARGO) fmt --manifest-path rust/substrate/Cargo.toml

tidy:
	$(GO) mod tidy

benchmark-routing:
	npx -y promptfoo@latest eval --config testdata/benchmark/routing/providers.yaml --output /tmp/polaris-benchmark-results.json
	$(GO) run ./cmd/polaris benchmark-routing /tmp/polaris-benchmark-results.json


gen-threshold-examples:
	env GOOS= GOARCH= $(GO) run tools/gen_threshold_examples.go configs/threshold-examples/

generate-manifest:
	env GOOS= GOARCH= $(GO) run tools/generate_manifest.go

all: tidy fmt lint test build gen-threshold-examples

# ─── 质量保障扩展 ─────────────────────────────────────────────────────────────

# deadcode: 死代码检查
deadcode:
	@$(GO) run golang.org/x/tools/cmd/deadcode@latest ./cmd/polaris/... > .deadcode.out || true
	@sed 's/:[0-9]*:[0-9]*:/:/g' .deadcode.out > .deadcode_clean.out
	@sed 's/ *#.*//' scripts/deadcode-allowlist.txt > .allowlist_clean.tmp
	@grep -vF -f .allowlist_clean.tmp .deadcode_clean.out > .deadcode_diff.out || true
	@if [ -s .deadcode_diff.out ]; then \
		echo "FAIL: Deadcode found:"; \
		cat .deadcode_diff.out; \
		rm .deadcode.out .deadcode_clean.out .deadcode_diff.out .allowlist_clean.tmp; \
		exit 1; \
	fi
	@rm .deadcode.out .deadcode_clean.out .deadcode_diff.out .allowlist_clean.tmp
	@echo "deadcode ok"

# test-race: 对并发密集路径运行 Go race detector
# 覆盖范围: Agent FSM / Worker Pool / MutationBus / 群体编排
# 为何不跑全量: race detector 慢 5-10x，仅针对并发高发区
test-race:
	$(GO) test -race -count=1 -timeout=120s \
		./internal/agent/... \
		./internal/memory/... \
		./internal/prompt/... \
		./internal/swarm/... \
		./internal/store/...

# rust-lint: Cargo clippy 静态分析（以 warning 为 error）
# 覆盖: 所有 target（lib + test + bench），FFI unsafe 代码
rust-lint:
	CFLAGS= LDFLAGS= $(CARGO) clippy \
		--all-targets \
		--manifest-path rust/substrate/Cargo.toml \
		-- -D warnings

# rust-audit: 检查 Cargo 依赖的已知 CVE
# 依赖: cargo-audit（若未安装则报错并给出安装命令）
rust-audit:
	@command -v cargo-audit >/dev/null 2>&1 || \
		{ echo "请先安装: cargo install cargo-audit"; exit 1; }
	$(CARGO) audit --manifest-path rust/substrate/Cargo.toml

# fuzz-taint: 运行 Taint 系统模糊测试
fuzz-taint:
	$(GO) test -fuzz=FuzzSanitizeToSafe ./internal/security/taint/... -fuzztime=30s
	$(GO) test -fuzz=FuzzNewTaintedString ./internal/security/taint/... -fuzztime=30s

# fuzz-skill: 运行 Skill 系统模糊测试
fuzz-skill:
	go test -fuzz=FuzzSkillValidationPipeline -fuzztime=30s ./internal/extension/skill/

# rust-deny: Cargo deny 静态分析（检查许可证、漏洞）
rust-deny:
	@command -v cargo-deny >/dev/null 2>&1 || \
		{ echo "请先安装: cargo install cargo-deny"; exit 1; }
	$(CARGO) deny --manifest-path rust/substrate/Cargo.toml check

# check-all: 完整质量门禁（CI 用）
# 顺序: fmt → lint → test → test-race → rust-lint → rust-test → rust-deny → deadcode
check-all: fmt lint test test-race rust-lint rust-test rust-deny deadcode
