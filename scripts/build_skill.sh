#!/usr/bin/env bash
# build_skill.sh — 编译单个 Skill 的 impl.go → impl.wasm
# 用法：./scripts/build_skill.sh <skill_name>
#   skill_name: skills/builtin/ 下的子目录名，如 regex_match

set -euo pipefail

GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

if [ $# -eq 0 ]; then
    echo "用法: $(basename "$0") <skill_name>"
    echo "示例: $(basename "$0") regex_match"
    exit 1
fi

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SKILL_NAME="$1"
SKILL_DIR="${PROJECT_ROOT}/skills/builtin/${SKILL_NAME}"

if [ ! -d "$SKILL_DIR" ]; then
    echo -e "${RED}Error: 技能目录不存在: ${SKILL_DIR}${NC}"
    exit 1
fi
if [ ! -f "${SKILL_DIR}/impl.go" ]; then
    echo -e "${RED}Error: ${SKILL_DIR}/impl.go 不存在${NC}"
    exit 1
fi

if ! command -v tinygo &>/dev/null; then
    echo "Error: tinygo 未找到。安装: https://tinygo.org/getting-started/"
    exit 1
fi

echo "=> 编译 ${SKILL_NAME} → impl.wasm"
# 子 shell 隔离 cd
(cd "$SKILL_DIR" && tinygo build -o impl.wasm -target=wasi impl.go)
echo -e "${GREEN}✓ ${SKILL_DIR}/impl.wasm${NC}"
