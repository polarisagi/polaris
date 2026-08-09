#!/usr/bin/env bash
# docs-refs.sh — 架构文档失效路径引用门控（HE-4 数据驱动 / CI 门控）。
#
# 背景：2026-07-28 架构文档审计发现 69 处「文档写的代码路径在仓库里不存在」，
# 成因是代码侧包拆分/文件迁移后文档未跟进（如 internal/protocol/interfaces.go
# 按域拆为 interfaces_*.go、internal/agent/dag → internal/execute/dag）。
# 这类漂移 100% 机械可检，靠人工复审必然复发——故照 ADR-0062 的 `make deadcode`
# 先例建立门控 + 白名单，不再依赖下一次全量审计。
#
# 扫描范围：docs/arch/*.md（活文档）+ 根 CLAUDE.md + internal/*/CLAUDE.md，
#           以及全仓 .go 注释（2026-08-08 并入，实现见 tools/comment_refs.go）。
# 判定对象：markdown 反引号里、以仓库顶层目录开头的路径字面量。
#
# 2026-08-09 并入 tools/anchor_refs.go：校验 "M13 §1.2" 这类模块章节锚点引用
# （与上面的路径字面量是两类判定对象，不能共用同一套正则——见该文件头部说明）。
# 采用 baseline 棘轮模式，白名单见 scripts/anchor-refs-baseline.txt。
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
	# 不设 maxdepth：2026-08-09 复核发现原来的 -maxdepth 3 恰好卡在现存最深的
	# internal/gateway/server/CLAUDE.md 上，再多一层子包就会被静默漏掉——
	# 「扫描面刚好够用」和「扫描面已经失效」在 CI 输出上长得一模一样。
	find internal -name 'CLAUDE.md' -type f
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
echo "docs-refs ok（活文档无失效路径引用）"

# .go 注释与文档侧的两类锚点门控都靠 go run 执行——若 go 不可用，此前的写法是
# 静默吞掉 env 报错继续往下跑，路径检查照常输出 ok，门控失效与门控通过在输出上
# 长得一模一样（2026-08-09 复核发现，登记于 local_playground/reports/plan-side-findings.md
# PS-006）。改为显式前置检查，缺 go 就明确报错退出，不再静默跳过。
if ! command -v go >/dev/null 2>&1; then
	echo "FAIL: 未找到 go 命令，无法执行 .go 注释 / § 锚点 / ADR 编号门控（tools/comment_refs.go、" \
		"tools/anchor_refs.go、tools/adr_index_check.go）。路径字面量检查已通过，但这三项未跑，" \
		"不能算 docs-refs 整体通过。" >&2
	exit 1
fi

# .go 注释侧的同类漂移（2026-08-08 并入）：判定规则相同、白名单共用同一份，
# 只是判定对象从 markdown 反引号换成 Go 注释。拆成 Go 程序的理由见该文件头。
env GOOS= GOARCH= go run tools/comment_refs.go || exit 1

# § 章节锚点漂移（2026-08-09 并入，见上方说明）。
env GOOS= GOARCH= go run tools/anchor_refs.go || exit 1

# ADR 编号体系自洽（2026-08-09 并入）：索引/已删除两表与实际文件三方对齐，
# 并拦截"编号被复用"这一不可变规则的违反。缘由见 tools/adr_index_check.go 头部。
# 注意：本项**要**扫 docs/arch/decisions/，与上面"不扫 ADR 正文路径"不冲突——
# 判定对象是编号体系而非正文里的历史路径，前者必须自洽，后者按定义允许陈旧。
env GOOS= GOARCH= go run tools/adr_index_check.go
