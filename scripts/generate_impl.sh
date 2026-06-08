#!/usr/bin/env bash
# generate_impl.sh — 为缺少 impl.go 的 Skill 目录生成 MVP stub
# 用法：./scripts/generate_impl.sh [skills_dir]

set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SKILLS_DIR="${1:-${PROJECT_ROOT}/skills/builtin}"

count=0
for dir in "$SKILLS_DIR"/*/; do
    [ -d "$dir" ] || continue
    if [ ! -f "${dir}impl.go" ]; then
        skill_name="$(basename "$dir")"
        cat > "${dir}impl.go" <<GOEOF
//go:build wasip1

package main

import (
	"encoding/json"
	"os"
)

func main() {
	input, err := os.ReadFile("/dev/stdin")
	if err != nil {
		os.Stderr.WriteString("read stdin: " + err.Error() + "\n")
		os.Exit(1)
	}

	var in map[string]any
	if err := json.Unmarshal(input, &in); err != nil {
		os.Stderr.WriteString("unmarshal: " + err.Error() + "\n")
		os.Exit(1)
	}

	out := map[string]any{
		"status":   "${skill_name} executed",
		"received": in,
	}
	enc, _ := json.Marshal(out)
	os.Stdout.Write(enc)
}
GOEOF
        echo "生成 ${dir}impl.go"
        count=$((count + 1))
    fi
done

echo "共生成 ${count} 个 stub"
