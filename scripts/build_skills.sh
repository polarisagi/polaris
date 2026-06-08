#!/usr/bin/env bash
# build_skills.sh — 批量将 skills/builtin/*/impl.go 编译为 impl.wasm
# 用法：./scripts/build_skills.sh [skills_dir]
#   skills_dir 默认为 skills/builtin

set -euo pipefail

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
NC='\033[0m'

# 脚本所在目录向上一级即项目根，保证从任意路径调用都正确
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SKILLS_DIR="${1:-${PROJECT_ROOT}/skills/builtin}"

if ! command -v tinygo &>/dev/null; then
    echo -e "${YELLOW}Warning: tinygo 未找到，跳过 Wasm 编译。${NC}"
    echo "安装 TinyGo: https://tinygo.org/getting-started/"
    exit 0
fi

if [ ! -d "$SKILLS_DIR" ]; then
    echo -e "${RED}Error: 目录不存在: ${SKILLS_DIR}${NC}"
    exit 1
fi

echo "=> 编译内置 Skills → Wasm  [${SKILLS_DIR}]"

SUCCESS_COUNT=0
FAIL_COUNT=0

# 使用 while read 而非 for $(find ...) 避免路径含空格时断裂
while IFS= read -r impl_file; do
    skill_dir="$(dirname "$impl_file")"
    skill_name="$(basename "$skill_dir")"
    echo "  -> $skill_name"
    # 用子 shell 隔离 cd，失败不影响主循环
    if (cd "$skill_dir" && tinygo build -o impl.wasm -target=wasi impl.go); then
        echo -e "     ${GREEN}✓ Success${NC}"
        SUCCESS_COUNT=$((SUCCESS_COUNT + 1))
    else
        echo -e "     ${RED}✗ Failed${NC}"
        FAIL_COUNT=$((FAIL_COUNT + 1))
    fi
done < <(find "$SKILLS_DIR" -name "impl.go" -type f)

echo ""
echo "=> 编译完成: ${GREEN}成功 ${SUCCESS_COUNT}${NC}"
if [ "$FAIL_COUNT" -gt 0 ]; then
    echo -e "            ${RED}失败 ${FAIL_COUNT}${NC}"
    exit 1
fi
