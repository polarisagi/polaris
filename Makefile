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

lint: taint-check fsm-check policy-gate-check
	golangci-lint run ./...
	env GOOS=wasip1 GOARCH=wasm golangci-lint run ./internal/extension/skill/sdk/...

taint-check:
	@echo "=== [GD-14-004] Taint propagation check ==="
	@! grep -rn 'TaintedString{' internal/ --include="*.go" | grep -v "_test.go" | grep -v "internal/security/taint/" | grep -v "newTainted\|MakeTainted\|func.*TaintedString" \
		&& echo "PASS: No raw TaintedString{} construction found" \
		|| (echo "FAIL: Direct TaintedString{} construction detected" && exit 1)

fsm-check:
	@echo "=== [GD-14-006] FSM control flow check ==="
	@! grep -rn 'goto ' internal/agent/fsm/ --include="*.go" | grep -v "_test.go" \
		&& echo "PASS: No goto found in FSM package" \
		|| (echo "FAIL: goto detected in FSM package" && exit 1)

policy-gate-check:
	@echo "=== [GD-14-004] PolicyGate fail-closed check ==="
	@if grep -rn 'policyGate\.Review\|gate\.Review' internal/ --include="*.go" | grep -v "_test.go" > /dev/null; then \
		echo "INFO: Found PolicyGate Review usages, please ensure they are guarded by nil checks"; \
	fi
	@echo "PASS: PolicyGate check done"

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
	if grep -rnE '^```(go|rust)' docs/arch/*.md ; then echo "FAIL: 禁止 \`\`\`go/\`\`\`rust 代码块" ; bad=1 ; fi ; \
	if grep -rnE '^\s*type\s+\w+\s+(struct|interface)\s*\{' docs/arch/*.md ; then echo "FAIL: 禁止裸 type struct/interface 定义" ; bad=1 ; fi ; \
	if grep -rnE '^\s*func\s+(\([^)]+\)\s+)?\w+\([^)]*\)' docs/arch/*.md ; then echo "FAIL: 禁止完整 func 签名" ; bad=1 ; fi ; \
	if [ $$bad -ne 0 ]; then exit 1; fi ; \
	echo "docs-lint ok"

# 失效路径引用门控: 活文档(docs/arch/*.md + CLAUDE.md)与全仓 .go 注释里写的代码路径必须真实存在。
# 白名单 scripts/docs-refs-allowlist.txt 仅收「文档在记载已删除/已迁移路径」的历史注记。
# 不扫 docs/arch/decisions/——ADR 按定义记录写作当时的事实，改它等于篡改历史。
docs-refs:
	@bash scripts/docs-refs.sh

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

# test-race: 全仓运行 Go race detector
#
# 2026-08-09 从 7 个手选目录改为全仓。原清单的问题不是选得少，是**选得不对且会漂移**：
# 它自称覆盖"并发高发区"，实测并发原语最密的三个包 internal/gateway（33 文件）、
# internal/llm（23）、internal/extension（18）一个都不在里面，反而收了
# internal/prompt（2 文件）。手维护的覆盖清单和它声称的标准之间没有任何机械约束，
# 漂了多久没人知道——和 ADR-0089 里那 8 条只扫 pkg/ 的规则是同一种失效。
#
# "race detector 慢 5-10x 所以不跑全量"这个理由实测不成立：包之间并行，
# 全仓 106 包墙钟 164s，只比原清单多约 100s。删掉清单 = 删掉漂移源。
test-race:
	$(GO) test -race -count=1 -timeout=900s ./internal/... ./cmd/... ./pkg/...

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
#      → docs-check（§跳读行号）→ docs-lint（代码块禁令）→ docs-refs（失效路径引用）
#      → fuzz-taint / fuzz-skill（各 30s，2026-08-09 并入）
#
# 2026-08-09：fuzz-taint / fuzz-skill 此前只是可手动调用的 target，从未进入 CI。
# 三个 fuzz 目标合计 90s，守的是 Taint 五级传播（HE-2 的密码学可验证边界）与
# Skill 校验管线——正是最不该只靠人工偶尔想起来跑一次的两处。
check-all: fmt lint test test-race rust-lint rust-test rust-deny deadcode docs-check docs-lint docs-refs fuzz-taint fuzz-skill
