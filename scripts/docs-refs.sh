#!/usr/bin/env bash
# docs-refs.sh — 架构文档失效路径引用门控（HE-4 数据驱动 / CI 门控）。
#
# 背景：2026-07-28 架构文档审计发现 69 处「文档写的代码路径在仓库里不存在」，
# 成因是代码侧包拆分/文件迁移后文档未跟进（如 internal/protocol/interfaces.go
# 按域拆为 interfaces_*.go、internal/agent/dag → internal/execute/dag）。
# 这类漂移 100% 机械可检，靠人工复审必然复发——故照 ADR-0062 的 `make deadcode`
# 先例建立门控 + 白名单，不再依赖下一次全量审计。
#
# 扫描范围：docs/arch/*.md（活文档）+ 根 CLAUDE.md + internal/*/CLAUDE.md。
# 判定对象：markdown 反引号里、以仓库顶层目录开头的路径字面量。
#
# 刻意不扫 docs/arch/decisions/（ADR）：ADR 是决策档案，按定义记录的是**写作当时**
# 的代码事实。事后把 ADR 正文里的旧路径改成新路径，等于篡改历史记录，会让
# 「为什么当初这么决策」失去可追溯的上下文。ADR 里的路径漂移是预期状态，不是缺陷。
#
# 刻意不报的三类（避免误报淹没真实缺陷）：
#   1. 带点的非文件名 basename —— Go 符号点记法（`pkg/concurrent.SafeGo`、
#      `internal/config.SandboxConfig`），扩展名不在已知白名单内即视为符号；
#   2. 白名单条目 —— 见 scripts/docs-refs-allowlist.txt（合法历史注记：
#      活文档正确记载"该路径已于某日删除/迁移，见 ADR-XXXX"，路径本就不该存在）；
#   3. 非顶层目录开头的相对路径 / URL / REST 端点。
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

ALLOWLIST="scripts/docs-refs-allowlist.txt"
# 已知文件扩展名：basename 命中这些才认定为"文件路径"，否则带点即判为符号点记法
KNOWN_EXT='go|rs|sql|md|toml|yaml|yml|json|txt|tmpl|sh|ps1|lock|mod|wasm|proto'
# 仓库顶层目录（引用必须以其中之一开头才纳入检查）
TOP_DIRS='internal|pkg|cmd|rust|configs|api|scripts|tools|web|docs'

mapfile -t FILES < <(
	find docs/arch -maxdepth 2 -name '*.md' -type f -not -path 'docs/arch/decisions/*'
	echo CLAUDE.md
	find internal -maxdepth 3 -name 'CLAUDE.md' -type f
)

bad=0
tmp_report=$(mktemp)
trap 'rm -f "$tmp_report"' EXIT

for f in "${FILES[@]}"; do
	[ -f "$f" ] || continue
	# 逐行取出反引号内、以顶层目录开头的路径字面量
	grep -noE "\`($TOP_DIRS)/[A-Za-z0-9_./-]+\`" "$f" 2>/dev/null |
		sed -E 's/`//g' |
		while IFS=: read -r line ref; do
			path="${ref%/}"
			[ -e "$path" ] && continue

			base="${path##*/}"
			# 带点但扩展名不在已知集合 → Go 符号点记法，不是路径
			if [[ "$base" == *.* ]] && ! [[ "$base" =~ \.($KNOWN_EXT)$ ]]; then
				continue
			fi
			# 白名单（整行精确匹配引用字符串）
			if [ -f "$ALLOWLIST" ] && grep -qxF "$ref" <(sed 's/[[:space:]]*#.*//; /^$/d' "$ALLOWLIST"); then
				continue
			fi
			echo "$f:$line: 引用了不存在的路径 -> $ref" >>"$tmp_report"
		done
done

if [ -s "$tmp_report" ]; then
	echo "FAIL: 架构文档存在失效路径引用（代码已迁移/删除但文档未跟进）:"
	echo
	sort -u "$tmp_report"
	echo
	echo "处理方式二选一："
	echo "  1) 路径确实迁移了 → 修正文档里的路径（首选）；"
	echo "  2) 该路径本就该不存在（文档在记载'已于某日删除/迁移'的历史注记）"
	echo "     → 把引用字符串整行加进 ${ALLOWLIST}，并在同行 # 注释里写明缘由。"
	bad=1
fi

if [ "$bad" -ne 0 ]; then
	exit 1
fi
echo "docs-refs ok（无失效路径引用）"
