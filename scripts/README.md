# scripts/

日常开发与系统维护的自动化脚本集合。

## 核心脚本清单

| 脚本 | 目标平台 | 说明与触发场景 |
|---|---|---|
| `install.sh` / `.ps1` | Mac/Linux/Win | **用户一键安装**：从远端下载最新版 Release 二进制，并配置系统的开机后台守护服务。 |
| `uninstall.sh` / `.ps1` | Mac/Linux/Win | **一键卸载清理**：停止并移除系统服务、删除二进制文件（默认安全保留 `~/.polarisagi/polaris` 下的所有用户数据）。 |
| `restart.sh` | 本地开发机 | **开发联调热启**：停止本地旧进程 → 重新构建前/后端代码 → 在 `28889` 开发测试端口启动程序。附加 `--full` 参数可强制重编底层 Rust FFI。 |
| `ci_test.sh` | CI 环境 | **全链路自动化测试**：由 GitHub Actions 自动触发，执行 Lint 检查、Go/Rust 单测及基准测试。 |
| `constitutional_review.sh`| CI 环境 | **AI 宪法审查**：PR 提交时触发，调用 LLM 严格依据 `CLAUDE.md` 架构准则对 Diff 代码进行违例拦截与审查。 |

---

## 构建工具链 (`tools/`)

> 注意：所有用 Go 语言编写的辅助构建工具均存放在项目根目录的 `tools/` 中，通过 `Makefile` 入口调用。

| make 快捷指令 | 对应 Go 工具 | 作用 |
|---|---|---|
| `make docs-sync` | `tools/sync_doc_toc.go` | 更新 `docs/arch/` 系列架构文档的 §跳读 行号锚点 |
| `make gen-threshold-examples` | `tools/gen_threshold_examples.go` | 自动从 Go 结构体生成 `configs/` 阈值配置文件示例 |
| `make generate-manifest` | `tools/generate_manifest.go` | 构建时自动生成系统内核的完整性校验哈希清单 |

---

## 附：配置 CI AI 审查环境变量

要使 `constitutional_review.sh` 生效，请在 GitHub 仓库 **Settings → Secrets and variables** 中预配：

| 变量 / Secret | 示例值 | 说明 |
|---|---|---|
| `REVIEWER_API_BASE` | `https://api.deepseek.com/v1` | OpenAI 兼容接口 BaseURL |
| `REVIEWER_API_KEY` | `sk-xxxxxx` | 厂商的 API 鉴权秘钥 |
| `REVIEWER_MODEL` | `deepseek-chat` | 调用的模型名称（留空默认 `deepseek-chat`） |
