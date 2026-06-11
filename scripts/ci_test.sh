#!/usr/bin/env bash
# ci_test.sh - 完整的本地 CI 测试验证脚本
# 该脚本复刻了 GitHub Actions (ci.yml) 中的全套流程，确保在 Push 之前可以本地提前发现问题。

set -e

# 获取脚本所在目录的上一级（项目根目录）
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "======================================"
echo "    Polaris 本地 CI 完整校验脚本      "
echo "======================================"

echo "[1/8] 准备环境: 创建 Mock Web dist..."
mkdir -p web/dist
touch web/dist/index.html

echo "[2/8] 执行 golangci-lint 静态检查..."
# 确保 golangci-lint 已安装
if ! command -v golangci-lint &> /dev/null; then
    echo "未找到 golangci-lint，正在尝试安装..."
    go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
fi
golangci-lint run ./...

echo "[3/8] 执行 docs/arch 一致性检查..."
make docs-check
make docs-lint

echo "[4/8] 编译 Rust Substrate 模块..."
make rust-build

echo "[5/8] 验证 Spec 一致性 (state.yaml SSoT)..."
go test -run "^TestSpec" ./internal/protocol/... -v

echo "[6/8] 运行 Go 全量单元测试 (带竞争检测与覆盖率分析)..."
go test ./pkg/... ./internal/... -v -race -coverprofile=coverage.out
go tool cover -func=coverage.out

echo "[7/8] 运行 Rust 全量单元测试与格式化检查..."
cargo test --manifest-path rust/substrate/Cargo.toml
cargo fmt --manifest-path rust/substrate/Cargo.toml --check

echo "[8/8] 验证 Eval Harness Gate..."
go run ./cmd/polaris eval --ci-gate

echo "======================================"
echo " ✅ 所有 CI 测试流程已顺利通过！"
echo " 您现在可以放心地推送到 GitHub。"
echo "======================================"
