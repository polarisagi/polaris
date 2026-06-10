# scripts/

开发工具脚本。日常操作通过 `make` 入口调用；Go 工具程序在 `tools/`。

| 脚本 | make 入口 | 说明 | 触发场景 |
|---|---|---|---|
| `restart.sh` | —（本地开发） | 停旧进程 → 构建前端 + Go 后端 → 重启 | 本地联调；`--full` 同时重编 Rust FFI |
| `constitutional_review.sh` | —（CI 触发） | 调用 OpenAI-compatible LLM 对 PR diff 做宪法违例审查 | PR 合入 main 时由 CI 自动执行 |

**tools/ 中的 Go 工具**（`//go:build ignore`，通过 `make` 调用）：

| 工具 | make 入口 | 说明 |
|---|---|---|
| `tools/sync_doc_toc.go` | `make docs-sync` / `make docs-check` | 刷新 `docs/arch/M*.md` §跳读 行号 |
| `tools/gen_threshold_examples.go` | `make gen-threshold-examples` | 从 Go 结构体生成 `configs/threshold-examples/m*.toml` |
| `tools/generate_manifest.go` | `make generate-manifest`（build 时自动） | 生成内核完整性校验清单 |

## constitutional_review.sh 配置

GitHub 仓库 **Settings → Secrets and variables** 中配置：

| 类型 | 名称 | 说明 |
|---|---|---|
| Secret | `REVIEWER_API_KEY` | API 密钥（任意 OpenAI-compatible 接口） |
| Variable | `REVIEWER_API_BASE` | API base URL |
| Variable | `REVIEWER_MODEL` | 模型名，留空默认 `deepseek-chat` |

常见 Provider：

```
# DeepSeek（项目推荐）
REVIEWER_API_BASE = https://api.deepseek.com/v1
REVIEWER_MODEL    = deepseek-chat

# OpenRouter
REVIEWER_API_BASE = https://openrouter.ai/api/v1
REVIEWER_MODEL    = deepseek/deepseek-chat-v3-0324

# OpenAI
REVIEWER_API_BASE = https://api.openai.com/v1
REVIEWER_MODEL    = gpt-4o-mini
```
