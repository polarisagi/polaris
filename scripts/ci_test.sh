#!/usr/bin/env bash
# ci_test.sh - 完整的本地 CI 测试验证脚本
# 该脚本复刻了 GitHub Actions (ci.yml) 中的全套流程，遇到错误不会立即中断，
# 而是会执行完所有步骤并在最后汇总报错信息，确保在 Push 之前可以本地提前发现所有问题。
# ./scripts/ci_test.sh

# 获取脚本所在目录的上一级（项目根目录）
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "======================================"
echo "    Polaris 本地 CI 完整校验脚本      "
echo "======================================"

# 用于收集失败步骤和日志的数组
FAILURES=()
FAILURE_LOGS=()

# 封装执行步骤的函数
run_step() {
    local step_name="$1"
    shift
    echo ""
    echo -e "\033[1;34m▶ $step_name...\033[0m"
    
    local log_file=$(mktemp)
    set -o pipefail
    if eval "$@" 2>&1 | tee "$log_file"; then
        set +o pipefail
        echo -e "\033[1;32m✅ 通过: $step_name\033[0m"
        rm -f "$log_file"
    else
        set +o pipefail
        echo -e "\033[1;31m❌ 失败: $step_name\033[0m"
        FAILURES+=("$step_name")
        FAILURE_LOGS+=("$log_file")
    fi
}

run_step "[1/10] 准备环境: 创建 Mock Web dist" "mkdir -p web/dist && touch web/dist/index.html"

# 确保 golangci-lint 已安装，并加入 PATH 环境变量
if ! command -v golangci-lint &> /dev/null; then
    echo "未找到 golangci-lint，正在尝试安装..."
    go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
    export PATH=$PATH:$(go env GOPATH)/bin
fi
run_step "[2/10] 执行静态检查 (make lint)" "make lint"

run_step "[3/10] 执行 docs/arch 一致性检查" "make docs-check && make docs-lint"

run_step "[4/10] 编译 Rust Substrate 模块" "make rust-build"

run_step "[5/10] 验证 Spec 一致性 (state.yaml SSoT)" "go test -run \"^TestSpec\" ./internal/protocol/... -v"

run_step "[6/10] 运行 Go 全量单元测试 (带竞争检测与覆盖率)" "go test ./pkg/... ./internal/... -v -race -coverprofile=coverage.out && go tool cover -func=coverage.out"

run_step "[7/10] 运行 Rust 单元测试与格式化检查" "make rust-test && cargo fmt --manifest-path rust/substrate/Cargo.toml --check"

run_step "[8/10] 执行全量编译 (make build)" "make build"

run_step "[9/10] 验证生成的配置是否最新" "make gen-threshold-examples && git diff --exit-code configs/threshold-examples/"

run_step "[10/10] 验证 Eval Harness Gate" "go run ./cmd/polaris eval --ci-gate"

echo ""
echo "======================================"
if [ ${#FAILURES[@]} -eq 0 ]; then
    echo -e "\033[1;32m 🎉 所有 CI 测试流程已顺利通过！\033[0m"
    echo -e "\033[1;32m 您现在可以放心地推送到 GitHub。\033[0m"
    echo "======================================"
    exit 0
else
    echo -e "\033[1;31m ❌ 本次 CI 校验未通过，发现以下流程执行失败：\033[0m"
    for i in "${!FAILURES[@]}"; do
        echo -e "\033[1;31m    - ${FAILURES[$i]}\033[0m"
        echo -e "\033[1;33m      ====== 错误日志摘要 ======\033[0m"
        tail -n 30 "${FAILURE_LOGS[$i]}" | sed 's/^/      /'
        echo -e "\033[1;33m      ==========================\033[0m"
        rm -f "${FAILURE_LOGS[$i]}"
    done
    echo ""
    echo -e "\033[1;33m 请根据上方日志修复报错信息后再尝试推送。\033[0m"
    echo "======================================"
    exit 1
fi
