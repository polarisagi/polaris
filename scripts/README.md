# scripts/

日常开发与系统维护的自动化脚本集合。

## 核心脚本清单

| 脚本 | 目标平台 | 说明与触发场景 |
|---|---|---|
| `install.sh` / `.ps1` | Mac/Linux/Win | **用户一键安装**：从远端下载最新版 Release 二进制，并配置系统的开机后台守护服务。 |
| `uninstall.sh` / `.ps1` | Mac/Linux/Win | **一键卸载清理**：停止并移除系统服务、删除二进制文件及 Rust dylib（默认安全保留 `~/.polarisagi/polaris` 下的所有用户数据）。 |
| `restart.sh` | 本地开发机 | **开发联调热启**：停止本地旧进程 → 重新构建前/后端代码 → 在 `28889` 开发测试端口启动程序。附加 `--full` 参数可强制重编底层 Rust FFI。 |
| `ci_test.sh` | 本地开发机 | **推送前本地全链路预检**：在本地复刻 GitHub Actions CI 的全套 13 步流程，遇错不立即中止、全部跑完后汇总报告。**在本机运行，不由 CI 自动触发**（CI 有自己的 `.github/workflows/ci.yml`）。 |
| `docs-refs.sh` | CI / 本地 | **架构文档失效路径门控**（`make docs-refs` 调用）：扫描活文档中引用但仓库内不存在的路径、Go 注释同类漂移、§ 锚点、ADR 编号体系自洽性。 |
| `constitutional_review.sh`| CI 环境 | **AI 宪法审查**：PR 提交时触发，调用 LLM 严格依据 `CLAUDE.md` 架构准则对 Diff 代码进行违例拦截与审查。 |
| `release-signing.sh` | 维护者本机 | **发布签名密钥管理**（ADR-0095）：`init` 开通 / `rotate` 轮换 / `status` 查看 / `verify` 离线验签 / `retire` 停用。**密钥不随发版轮换**——一把密钥签所有 release，流水线自动完成。脚本不含任何秘密（它生成密钥并推进 GitHub Secrets），私钥从不落进仓库。 |
| `review-prescan.sh` | 本地开发机 | **架构审核预扫描**：在每轮代码审核前自动生成五份机械事实卡片（仓库体量/ADR 清单/模块行数/失效路径候选/标识符漂移），输出到 `local_playground/reports/arch-audit/facts/`。卡片是候选不是结论，复核与归因由 LLM 完成。 |

---

## 辅助数据文件

| 文件 | 说明 |
|---|---|
| `review-batch-scope.txt` | 审核批次范围表（机器可读）：`tools/review_check.go` 的 C6 门控读取此文件，判定某批次范围内哪些文件在报告里去向不明。**改批次表时必须同步改这里**。 |
| `rejected-proposals.txt` | ROADMAP §5「主动决议拒绝清单」的机械索引：`review_check` 的 C13 对每条 GD 标题做精确子串匹配，命中即报红并打回拒绝理由，防止同一提案被反复重提。 |

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
