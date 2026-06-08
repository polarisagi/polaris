.PHONY: build run test lint clean rust-build rust-test build-skills build-skill build-ui dev-ui docs-sync docs-check docs-lint gen-threshold-examples generate-manifest _copy-skills

GO := go
CARGO := cargo
BINARY := polaris
WEBUI_DIR := web

build: generate-manifest rust-build build-ui
	@mkdir -p bin/lib
	@cp rust/substrate/target/release/libsubstrate.dylib bin/lib/ 2>/dev/null || true
	@cp rust/substrate/target/release/libsubstrate.so bin/lib/ 2>/dev/null || true
	@cp rust/substrate/target/release/substrate.dll bin/lib/ 2>/dev/null || true
	$(GO) build -o bin/$(BINARY) ./cmd/polaris
	@$(MAKE) --no-print-directory _copy-skills

build-tier1: generate-manifest rust-build-tier1 build-ui
	@mkdir -p bin/lib
	@cp rust/substrate/target/release/libsubstrate.dylib bin/lib/ 2>/dev/null || true
	@cp rust/substrate/target/release/libsubstrate.so bin/lib/ 2>/dev/null || true
	@cp rust/substrate/target/release/substrate.dll bin/lib/ 2>/dev/null || true
	$(GO) build -tags tier1 -o bin/$(BINARY) ./cmd/polaris
	@$(MAKE) --no-print-directory _copy-skills

# 将已编译的 wasm 复制到 bin/skills/ 使二进制可独立运行（不依赖 CWD）
_copy-skills:
	@if [ -d skills/builtin ]; then \
		for d in skills/builtin/*/; do \
			name=$$(basename "$$d"); \
			wasm="$$d/impl.wasm"; \
			if [ -f "$$wasm" ]; then \
				mkdir -p "bin/skills/$$name"; \
				cp "$$wasm" "bin/skills/$$name/impl.wasm"; \
			fi; \
		done; \
	fi

build-ui:
	@cd $(WEBUI_DIR) && npm install --silent && npm run build

dev-ui:
	@cd $(WEBUI_DIR) && npm install --silent && npm run dev

run:
	$(GO) run ./cmd/polaris

test:
	$(GO) test ./pkg/... ./internal/...

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/ bin/lib
	$(CARGO) clean --manifest-path rust/substrate/Cargo.toml

# 重写 docs/arch/*.md 头部 §跳读 行号 (从实际 ## headers 同步)
docs-sync:
	$(GO) run tools/sync_doc_toc.go

# CI 用: 校验 §跳读 与实际 headers 一致, drift 时退出非零
docs-check:
	$(GO) run tools/sync_doc_toc.go -check

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
	$(CARGO) build --release --manifest-path rust/substrate/Cargo.toml

rust-build-tier1:
	$(CARGO) build --release --features tier1 --manifest-path rust/substrate/Cargo.toml

rust-test:
	$(CARGO) test --manifest-path rust/substrate/Cargo.toml

fmt:
	$(GO) fmt ./...
	$(CARGO) fmt --manifest-path rust/substrate/Cargo.toml

tidy:
	$(GO) mod tidy

benchmark-routing:
	npx promptfoo@latest eval --config testdata/benchmark/routing/providers.yaml --output /tmp/polaris-benchmark-results.json
	$(GO) run ./cmd/polaris benchmark-routing /tmp/polaris-benchmark-results.json

build-skills:
	@./scripts/build_skills.sh

# 编译单个技能: make build-skill SKILL=regex_match
build-skill:
	@./scripts/build_skill.sh $(SKILL)

gen-threshold-examples:
	$(GO) run tools/gen_threshold_examples.go configs/threshold-examples/

generate-manifest:
	$(GO) run tools/generate_manifest.go

all: tidy fmt lint test build build-skills gen-threshold-examples
