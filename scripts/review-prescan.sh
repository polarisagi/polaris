#!/usr/bin/env bash
# review-prescan 生成审核轮的机械事实卡片。
#
# 立此脚本的原因：这 5 份卡片原本以 ~150 行 shell 代码块的形式内嵌在审核提示词里，
# 由 LLM 照抄到终端执行。后果有三：提示词里 150 行与审核判断力无关的内容持续挤占
# 注意力预算；每轮照抄都可能抄错或跳过；命令本身无法被版本管理单独 review。
# 凡是 grep 能判的就别让 LLM 判，凡是脚本能跑的就别让 LLM 抄。
#
# 产出 local_playground/reports/arch-audit/facts/：
#   00-repo-facts.md         文档体量 / ADR 清单与 README 状态 / 各模块非测试 Go 行数 / DDL / state.yaml 顶层键
#   01-broken-refs.txt       文档写了但仓库里不存在的路径（候选）
#   02-adr-xrefs.txt         每个 ADR 被谁引用（「引用方核查」直接消费，勿再全仓 grep）
#   03-adr-index-drift.txt   实体文件 ⇄ README 索引 ⇄ CLAUDE.md 三方差集 + 编号缺口
#   04-doc-symbols-missing.txt 文档提到但代码中找不到的标识符（候选，噪音较高）
#
# 这五份是**候选不是结论**：复核真伪 + 归因 + 定级是 LLM 的活，重新找一遍不是。
# 预扫抓到的条目不计入审核成果——它们零成本可得。
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

FD=local_playground/reports/arch-audit/facts
mkdir -p "$FD" local_playground/reports/arch-audit/logs
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# ---- 00 事实卡片 ----
{
  echo "# 仓库事实卡片（scripts/review-prescan.sh 生成，勿手改）"
  echo; echo "生成时间: $(date '+%F %T')  HEAD: $(git rev-parse --short HEAD)"
  echo; echo "## 架构文档清单（行数 / 字节）"; echo
  echo '| 文档 | 行数 | 字节 |'; echo '|---|---|---|'
  for f in docs/arch/*.md docs/arch/spec/state.yaml; do
    [ -f "$f" ] && printf '| %s | %s | %s |\n' "$f" "$(wc -l < "$f" | tr -d ' ')" "$(wc -c < "$f" | tr -d ' ')"
  done
  echo; echo "## ADR 清单（行数 + README 索引状态列）"; echo
  echo '| 编号 | 文件 | 行数 | README 状态 |'; echo '|---|---|---|---|'
  for f in docs/arch/decisions/ADR-0*.md; do
    pad=$(basename "$f" | sed -E 's/^ADR-([0-9]{4})-.*/\1/')
    st=$(awk -F'|' -v n="$pad" 'NF>3 { gsub(/^ *| *$/,"",$2); if ($2==n) { gsub(/^ *| *$/,"",$4); print $4; exit } }' docs/arch/decisions/README.md)
    [ -n "$st" ] || st='(README 无索引行!)'
    printf '| %s | %s | %s | %s |\n' "$pad" "$(basename "$f")" "$(wc -l < "$f" | tr -d ' ')" "$st"
  done
  echo; echo "## 各模块非测试 Go 行数"; echo
  echo '| 模块 | 文件数 | 行数 |'; echo '|---|---|---|'
  for d in internal/*/ pkg/ cmd/; do
    files=$(git ls-files "$d**/*.go" | grep -v '_test\.go$' | grep -v '/pb/' || true)
    [ -n "$files" ] || continue
    printf '| %s | %s | %s |\n' "$d" "$(echo "$files" | wc -l | tr -d ' ')" "$(echo "$files" | tr '\n' '\0' | xargs -0 cat 2>/dev/null | wc -l | tr -d ' ')"
  done
  echo; echo "## DDL Schema 文件"; echo
  git ls-files 'internal/protocol/schema/*.sql' | sed 's/^/- /'
  echo; echo "## state.yaml 顶层键（供按 § 偏移局部读）"; echo
  grep -nE '^[a-zA-Z_][a-zA-Z0-9_]*:' docs/arch/spec/state.yaml | sed 's/^/- /'
  echo; echo "## 存在 CLAUDE.md 的模块"; echo
  git ls-files 'internal/*/CLAUDE.md' 'internal/*/*/CLAUDE.md' | sed 's/^/- /'
} > "$FD/00-repo-facts.md"

# ---- 01 失效路径引用 ----
for doc in docs/arch/*.md docs/arch/decisions/*.md CLAUDE.md docs/arch/spec/state.yaml; do
  [ -f "$doc" ] && grep -noE '(internal|pkg|cmd|configs|rust|api|scripts|web|tools)/[A-Za-z0-9_./-]+' "$doc" \
    | sed -E "s#^([0-9]+):#$doc:\1:#" || true
done > "$TMP/refs.txt"
sed -E 's/^.*:[0-9]+://; s/[.,)]+$//' "$TMP/refs.txt" | grep -v '\*' | grep -vE '[_/]$' | sort -u \
  | while read -r p; do [ -e "$p" ] || echo "$p"; done > "$TMP/missing.txt"
{
  echo "# 文档中引用但仓库内不存在的路径（候选，需复核归因）"
  echo "# 格式: <文档>:<行号>: <失效路径>；已过滤通配符与被中文截断的残缺引用，仍可能有占位示例误报"
  echo
  while read -r p; do
    grep -F ":$p" "$TMP/refs.txt" | sed -E "s#(:[0-9]+):.*#\1: $p#" | sort -u
  done < "$TMP/missing.txt"
} > "$FD/01-broken-refs.txt"

# ---- 02 ADR 引用图谱 ----
git grep -noE 'ADR-[0-9]{4}' -- ':!local_playground' \
  | awk -F: '{ n=$NF; sub(/:[^:]*$/,"",$0); print n"\t"$0 }' | sort -u > "$TMP/x.txt"
{
  echo "# 每个 ADR 编号在仓库中的引用点（排除 local_playground）"
  echo "# 「引用方核查」直接消费本文件。[self] = ADR 自身文件内的出现，判定时忽略。"
  echo
  for f in docs/arch/decisions/ADR-0*.md; do
    num=$(basename "$f" | sed -E 's/^(ADR-[0-9]{4}).*/\1/')
    self="docs/arch/decisions/$(basename "$f")"
    cnt=$(awk -F'\t' -v n="$num" -v s="$self" '$1==n && index($2,s)!=1' "$TMP/x.txt" | wc -l | tr -d ' ')
    echo "== $num  外部引用点: $cnt =="
    awk -F'\t' -v n="$num" -v s="$self" '$1==n { if (index($2,s)==1) print "  [self] "$2; else print "  "$2 }' "$TMP/x.txt"
    echo
  done
} > "$FD/02-adr-xrefs.txt"

# ---- 03 ADR 索引三方差集 ----
ls docs/arch/decisions/ADR-0*.md | sed -E 's#.*/ADR-([0-9]{4})-.*#\1#' | sort -u > "$TMP/A"
awk '/^## 索引/{f=1;next} /^## 已删除/{f=0} f' docs/arch/decisions/README.md \
  | awk -F'|' 'NF>3 { gsub(/^ *| *$/,"",$2); if ($2 ~ /^0[0-9]{3}$/) print $2 }' | sort -u > "$TMP/B"
awk '/^## 已删除/{f=1} f' docs/arch/decisions/README.md \
  | awk -F'|' 'NF>3 { gsub(/^ *| *$/,"",$2); if ($2 ~ /^0[0-9]{3}$/) print $2 }' | sort -u > "$TMP/C"
{
  echo "# ADR 索引差集（差集非空 = 确定性缺陷，只需确认与定级）"; echo
  echo "# 注：不做「CLAUDE.md 是否收录该 ADR」的差集——根 CLAUDE.md 已明确声明不维护 ADR 索引副本"
  echo "#（编号 SSoT = decisions/README.md），照旧做会产出 38 条按设计成立的假阳性。"
  echo "# 前三项与 tools/adr_index_check.go 门控重叠，此处保留是因为零成本且能与 04 卡片交叉判读。"; echo
  echo "实体文件 $(wc -l < "$TMP/A" | tr -d ' ')  README 现行索引 $(wc -l < "$TMP/B" | tr -d ' ')  已删除表 $(wc -l < "$TMP/C" | tr -d ' ')"; echo
  echo "## [P1] 有实体文件但 README 索引表无对应行"; comm -23 "$TMP/A" "$TMP/B" | sed 's/^/  ADR-/'; echo
  echo "## [P1] README 索引有行但无实体文件（已剔除「已删除」表编号）"; comm -13 "$TMP/A" "$TMP/B" | comm -23 - "$TMP/C" | sed 's/^/  ADR-/'; echo
  echo "## [P0] 同时出现在实体文件与「已删除」表（矛盾，违反编号不复用）"; comm -12 "$TMP/A" "$TMP/C" | sed 's/^/  ADR-/'; echo
  echo "## 编号缺口（核对「刻意跳号」说明是否完整）"
  awk 'BEGIN{p=0}{n=$1+0; if(p&&n>p+1) for(i=p+1;i<n;i++) printf "  ADR-%04d 缺号\n", i; p=n}' "$TMP/A"
} > "$FD/03-adr-index-drift.txt"

# ---- 04 文档提到但代码中不存在的标识符 ----
grep -hoE '`[A-Z][A-Za-z0-9]{4,}`' docs/arch/*.md docs/arch/decisions/*.md \
  | tr -d '`' | grep -E '[A-Z].*[A-Z]' | sort -u > "$TMP/cand"
git grep -hoE '[A-Z][A-Za-z0-9]{4,}' -- internal pkg rust cmd configs api | sort -u > "$TMP/code"
comm -23 "$TMP/cand" "$TMP/code" > "$TMP/sym_missing"
{
  echo "# 文档中出现但在 internal/ pkg/ rust/ cmd/ configs/ api/ 中找不到的标识符候选"
  echo "# 噪音较高（英文术语、外部库名、纯概念名会误入），需逐条复核。真阳性 = 组件存在性漂移。"
  echo "# 候选 $(wc -l < "$TMP/cand" | tr -d ' ') 个，其中代码中缺失 $(wc -l < "$TMP/sym_missing" | tr -d ' ') 个。"
  echo
  while read -r sym; do
    echo "$sym"
    grep -lF "\`$sym\`" docs/arch/*.md docs/arch/decisions/*.md | sed 's/^/    出现于: /' || true
  done < "$TMP/sym_missing"
} > "$FD/04-doc-symbols-missing.txt"

echo "review-prescan 完成 → $FD"
printf '  %-28s %s 行\n' \
  "00-repo-facts.md" "$(wc -l < "$FD/00-repo-facts.md" | tr -d ' ')" \
  "01-broken-refs.txt" "$(wc -l < "$FD/01-broken-refs.txt" | tr -d ' ')" \
  "02-adr-xrefs.txt" "$(grep -c '^== ' "$FD/02-adr-xrefs.txt")" \
  "03-adr-index-drift.txt" "$(wc -l < "$FD/03-adr-index-drift.txt" | tr -d ' ')" \
  "04-doc-symbols-missing.txt" "$(wc -l < "$FD/04-doc-symbols-missing.txt" | tr -d ' ')"
