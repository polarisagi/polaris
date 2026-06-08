#!/usr/bin/env bash
# Constitutional Review — PR 触发的独立 AI 宪法审查
# 设计依据: docs/arch/decisions/ADR-0014-adversarial-review-action.md
#
# 必备环境变量:
#   REVIEWER_API_KEY   - OpenAI-compatible API 密钥（DeepSeek / OpenRouter / OpenAI 等）
#   REVIEWER_API_BASE  - API base URL（如 https://api.deepseek.com/v1）
#   PR_NUMBER          - PR 号（用于 gh pr comment）
#   REPO               - owner/repo 格式
#   GH_TOKEN           - GitHub token（gh CLI 自动使用）
# 可选:
#   REVIEWER_MODEL     - 默认 deepseek-chat；不可与开发者用模型同型号

set -euo pipefail

CONSTITUTION="docs/specs/00-Constitution.md"
DIFF_FILE="/tmp/pr.diff"
MAX_DIFF_BYTES=100000  # 100KB 截断阈值，防 prompt 爆 context
MODEL="${REVIEWER_MODEL:-deepseek-chat}"
API_BASE="${REVIEWER_API_BASE:-}"
API_KEY="${REVIEWER_API_KEY:-}"

# ─── 前置检查 ────────────────────────────────────────────────────────────────

if [ ! -f "$CONSTITUTION" ]; then
  echo "ERROR: $CONSTITUTION 不存在；reviewer 无法加载宪法"
  exit 1
fi
if [ ! -f "$DIFF_FILE" ]; then
  echo "ERROR: $DIFF_FILE 未生成（workflow 'Generate Diff' 步骤可能失败）"
  exit 1
fi
if [ -z "$API_KEY" ]; then
  echo "ERROR: REVIEWER_API_KEY 未设置（支持任意 OpenAI-compatible 接口）"
  exit 1
fi
if [ -z "$API_BASE" ]; then
  echo "ERROR: REVIEWER_API_BASE 未设置（示例: https://api.deepseek.com/v1）"
  exit 1
fi

# ─── Diff 截断（防 prompt 爆）─────────────────────────────────────────────────

DIFF_SIZE=$(wc -c < "$DIFF_FILE")
DIFF_TRUNCATED="false"
if [ "$DIFF_SIZE" -gt "$MAX_DIFF_BYTES" ]; then
  echo "WARN: diff $DIFF_SIZE bytes > $MAX_DIFF_BYTES 阈值，截取头部"
  head -c "$MAX_DIFF_BYTES" "$DIFF_FILE" > "${DIFF_FILE}.truncated"
  DIFF_FILE="${DIFF_FILE}.truncated"
  DIFF_TRUNCATED="true"
fi

# ─── 构建提示词 ──────────────────────────────────────────────────────────────

CONSTITUTION_TEXT=$(cat "$CONSTITUTION")
DIFF_TEXT=$(cat "$DIFF_FILE")

# 严格提示词：只输出违例或 NONE，禁止建议/表扬/推理
SYSTEM_PROMPT="你是 polaris 项目的宪法审查者。审查以下 PR diff，对照宪法规则逐条违例报告。

严格要求:
1. 仅输出违例（如有）；无违例则输出唯一一行 \"NONE\"
2. 每条违例格式: \"R<编号> | <文件:行号> | <一句话说明违规>\"  或 \"B<编号> | ...\"
3. 不输出建议、不输出表扬、不输出推理过程——只违例
4. 如违例属于已记录 ADR 豁免（如 ADR-0001 R1.3 一等公民指标），跳过该条
5. 不知道的事不假设——只判断 diff 内可见内容
6. 优先级: R1 反模式 > R7 可读性硬上限 > B1 层依赖 > R5 注释规范

宪法规则全文:
$CONSTITUTION_TEXT"

USER_MESSAGE="PR diff:

$DIFF_TEXT"

# ─── 调用 OpenAI-compatible API ──────────────────────────────────────────────

REQUEST=$(jq -n \
  --arg model "$MODEL" \
  --arg system "$SYSTEM_PROMPT" \
  --arg user "$USER_MESSAGE" \
  '{
    model: $model,
    max_tokens: 4096,
    messages: [
      {role: "system", content: $system},
      {role: "user", content: $user}
    ]
  }')

RESPONSE=$(curl -sS "${API_BASE}/chat/completions" \
  -H "Authorization: Bearer $API_KEY" \
  -H "content-type: application/json" \
  -d "$REQUEST" || echo '{"choices":[{"message":{"content":"ERROR: API call 失败"}}]}')

REVIEW=$(echo "$RESPONSE" | jq -r '.choices[0].message.content // "ERROR: 无响应内容"')

# ─── 输出与 PR comment ───────────────────────────────────────────────────────

TRUNCATE_NOTE=""
if [ "$DIFF_TRUNCATED" = "true" ]; then
  TRUNCATE_NOTE=$'\n> ⚠ Diff 超过 '${MAX_DIFF_BYTES}$' 字节，仅审查头部'
fi

COMMENT_BODY="## 🔱 Constitutional Review (Independent AI)

> ADR-0014 执行带 3 对抗审查 | reviewer model: \`$MODEL\` | warning-only（不阻断 CI）${TRUNCATE_NOTE}
> 仅检查宪法 R1-R8 / B1-B5 / R6 反模式与硬约束。建议性意见不在范围；人类 review 仍是最终裁定。

\`\`\`
$REVIEW
\`\`\`
"

# 完整输出到 CI logs（可审计）
echo "$COMMENT_BODY"

# Post 到 PR comment（失败仅 warning，不影响后续）
if [ -n "${PR_NUMBER:-}" ] && [ -n "${REPO:-}" ]; then
  if echo "$COMMENT_BODY" | gh pr comment "$PR_NUMBER" --repo "$REPO" --body-file -; then
    echo "✓ PR comment posted"
  else
    echo "::warning::gh pr comment 失败，审查结果仅在 CI logs 可见"
  fi
fi

# 决策日志（不 exit 1）
if echo "$REVIEW" | grep -q "^NONE$"; then
  echo "✓ No constitutional violations detected"
else
  echo "⚠ Constitutional violations reported (warning-only per ADR-0014)"
fi

exit 0
