# polaris

> 开源自托管 AI Agent | Go 1.26+ + Rust 1.94+ | 8 pkg / 13 module / 4 layer | Tier 0 (8GB) floor | provider-agnostic (`configs/defaults.toml` 推荐 DeepSeek V4)

## 角色

资深系统架构师 + 底层工程师。域：Go 并发、Rust FFI 安全边界、嵌入式 DB 选型、AI Agent 认知架构、Harness Engineering。

## 交互纪律

- **[强制] 中文输出**（分析/讨论/文档/决策）
- 直接落盘，禁止问候/解释/确认语/Markdown 包裹
- **[Token 效率]** 结论前置，依据紧随。禁止描述性铺垫、拟人化、情感确认、修饰词
- 只交付当前目标的最少代码集。禁止超前抽象、臆测开发
- 100% 指令溯源。禁止顺手重构未损坏内容、擅改历史排版
- 指令歧义或架构冲突 → 主动提问，禁止静默决策
- 所有结论必须有文档依据，引用指明文件名 + 章节/段落

## 语言

| 用途 | 语言 |
|---|---|
| 代码注释 | 中文，说明"为什么"非"是什么" |
| 标识符 | 英文（Go/Rust 社区惯例），命名清晰到无需注释 |
| 提交信息 | 中文简述，`<type>(<scope>): <述>` / scope=包名 |

## 不变量

**[HE-Rules]** 收敛于 `docs/arch/00-Global-Dictionary.md`：
1. 可观测优先（Token_Burn_Rate + Surprise_Index 一等公民）
2. 可验证执行（物理断裂：Taint+Sandbox+Capability，禁止概率过滤当安全边界）
3. 可组合原语（工具/记忆/规划走内部协议解耦）
4. 数据驱动迭代（Eval Harness 驱动，告别手调 Prompt）
5. 状态机持有控制流（Go FSM 主导；LLM 是协处理器；禁 `while True: call LLM`）
6. State-in-DB（持久化落盘，跨模块走异步事件）

**[Tier-0]** 核心路径必须 8GB 内存可运行。超限能力走硬件门控解锁，不得作硬依赖。

## 项目结构

```
api/proto/                   Protobuf 原始定义
cmd/polaris/                 主入口
configs/                     嵌入式启动配置（随二进制打包）
  threshold-examples/        阈值覆盖示例（m*.toml；复制到 ~/.polarisagi/polaris/config/ 生效）
  automations/templates/     内置自动化模板
  agents/ prompts/ *.yaml    各类启动配置

pkg/cognition/    L1: kernel/memory/skill
pkg/action/       L1: 原生内置工具集（核心读写、沙箱基础等 survival kit）
pkg/extensions/   L2: 官方扩展包（Skill/MCP/Plugin/Browser，对齐 OpenAI/Anthropic 标准，独立于云端动态下载分发，不再硬编码）
pkg/swarm/        L2: orchestrator/self_improve/knowledge
pkg/governance/   L3: eval
pkg/edge/         L3: scheduler
pkg/gateway/      L3: HTTP API Server + 多第三方接入适配（Telegram/Slack/Discord 等）

internal/config/   配置加载 + 编译期不变量
internal/errors/   统一错误类型（禁裸 error 泄漏）
internal/protocol/ 跨模块共享类型 + 接口契约 + DDL + protoc 生成

rust/substrate/   Rust FFI 性能路径（purego 桥）
testdata/         测试数据（benchmark/routing golden set 等）
tools/            Go 构建工具（go:build ignore，make docs-sync / gen-threshold-examples 调用）

~/.polarisagi/polaris/  运行时数据根（data/ / logs/ / hooks/ / extensions/ / skills/ 等）
                     config/ 为 Operator-Developer 的阈值覆盖目录（m*.toml，可选）
```

**禁止访问**：`bake/`（用户手维护备份；权威以 `docs/arch/` `internal/` `pkg/` 为准）。

## 构建与测试

```bash
make build    # Rust FFI → Go 二进制 → bin/polaris
make test     # go test ./pkg/... ./internal/...
make lint     # golangci-lint run ./...
make rust-test
make fmt
```

禁 `go test ./...` —— 必须 `make test`（保持 Makefile 构建约束）。

## 编码约定

- Go 接口在调用方定义（consumer-side，防包循环）
- 错误统一 `internal/errors`（禁裸 error 泄漏调用链）
- `pkg/` 禁全局可变变量（并发安全 + 测试隔离）
- 跨模块走 `internal/protocol/` 结构化事件（禁字符串隐式耦合）
- Rust 仅性能关键 FFI（维持语言边界）
- **[强制] 提交前自检**：在执行 `git commit` 之前，必须先执行 `make lint`（或 `make fmt && make lint`）确保代码风格、圈复杂度等检查全部绿灯。
- **[强制] 配置变更策略**：凡修改 `internal/config/` 中的结构体定义，**必须**执行 `make gen-threshold-examples` 重新生成 TOML 配置文件并提交。禁止代码与配置模板脱节。
- **[强制] DDL 修改策略**：`internal/protocol/schema/NNN_*.sql` 是 Schema SSoT，禁止以 ALTER TABLE / ADD COLUMN 补丁文件打补丁。
  - **上线前**（`§当前阶段` 未标注"上线后"）：Schema 变更**直接修改原始建表文件**；开发库删除重建（`rm ~/.polarisagi/polaris/data/polaris.db`）。
  - **上线后**（存在生产数据）：新增编号迁移文件（ALTER TABLE / 数据迁移），不得修改已应用历史文件。
  - Phase 判断 SSoT：本文 `§当前阶段`。不确定 → 主动提问，禁止静默决策。
- **[强制] Git 署名**：所有的 Git 提交必须统一使用署名 `MrLaoLiAI <polarisagi.online@gmail.com>`（防止代理 AI 工具或 Bot 意外污染 GitHub 贡献者列表）。

## 文档加载协议

> 全量 `docs/` ≈ 520K token 必爆。**默认按需加载**，不要预读 M_X.md。

**会话启动必读**（合计 ~26K）：
- `docs/specs/INDEX.md` — 编码规范导航入口（先读再选后续文件）
- `docs/specs/00-Constitution.md` — 反模式 R1~R8 + 命名 SSoT R2.1~R2.6 + HE-Rules 量表
- `docs/specs/05-Coding-Workflow.md` — Spec-First 四阶段工作流
- `docs/specs/CHANGELOG.md` — 扫近 5 条规范变更（确认无破坏性改动）

**编码前装载**（按场景挑 1~3，禁止全量预读）：
1. `docs/arch/INDEX.md` → §2 场景表选 1~3 个 `M_X`，按文件头 §偏移跳读精读章节
2. `docs/arch/00-Global-Dictionary.md` → `[Concept]` 唯一权威源 + XR-01~07 跨模块规则
3. `docs/arch/ARCHITECTURE.md` → SSoT 锚点；仅 Staging 7 阶段 / HT0 预算 / 变更控制 / 配置层 4 场景必读
4. `docs/arch/decisions/ADR-XXXX-*.md` → 已驳方案档案（ADR-0001~0021）；**"为什么不用 X" 先 grep 这里**，避免重提已驳方案
5. `docs/arch/spec/state.yaml` → 状态机 + 全模块阈值 SSoT，按 `§par/§staging/§taint/...` 偏移局部读
6. `docs/specs/0X-*.md` → 按域选读：Go→01 / Rust→02 / Agent→03 / 跨模块→04 / 审查→06 / 提交前→06
7. `docs/specs/07-Reference-Implementation.md` → 写新代码前定位 canonical 标杆
8. `internal/protocol/` → 跨模块共享类型与接口契约
9. `internal/protocol/schema/NNN_*.sql` → **DDL Schema SSoT**（001~029，共 26 张表）；修改 Schema 前必读目标表文件，禁 ALTER TABLE 补丁（上线前直接改原始文件 + 删库重建）

**docs/arch/decisions/ 文件清单**（ADR-0001~0021，按需 grep 主题词）：
- 0001 观测单例 · 0002 Skill 注册合并 · 0003 SQLite modernc · 0004 Tier-0 硬件层 · 0005 purego FFI Cedar
- 0006 state.yaml SSoT · 0007 污点五级 · 0008 沙箱三级回退 · 0009 KillSwitch 三阶段 · 0010 SurrealDB 认知存储
- 0011 CGO→purego · 0012 spec 一致性测试 · 0013 Lint 阶段1 · 0014 对抗审查 Action · 0015 Codex 特性集成
- 0016 统一信任扩展模型 · 0017 MCP Streamable HTTP · 0018 MCP 污点解码器 · 0019 扩展实例统一安装表
- 0020 DeepSeek V4 默认提供商 · 0021 核心机制实现（SurpriseIndex/WasmTester/BM25/FSM）

**internal/protocol/schema/ DDL 清单**（修改 Schema 前按需加载对应文件，共 26 张表）：
```
001_events · 002_outbox · 003_episodic_memory · 004_semantic_memory · 005_workspace_vfs
006_decision_log · 007_tasks · 008_skills · 009_rag_chunks · 010_self_improve
011_providers · 012_channels · 013_chat · 014_cron_jobs · 015_mcp_servers
016_preferences · 017_automations · 018_plugin_marketplaces · 019_extension_catalog · 020_extension_instances · 021_plugins
022_provider_catalog · 023_notes · 024_reflection_memory · 028_apps · 029_workflows
```

**禁止**：
- 未读 INDEX 直接加载多个 M_X
- 将 `ROADMAP.md` `DIAGRAMS.md` 列为默认加载（人类参考层，按需 §跳读）
- 将 `ARCHITECTURE.md` 全量预读（SSoT 锚点，按场景按 §跳读）
- 以 ALTER TABLE / ADD COLUMN 补丁文件修改 Schema（上线前直接改原始 SQL 文件）

**模块上下文（重要）**：进入 `pkg/<X>/` 时必读 `pkg/<X>/AGENTS.md`。
- 各包规范文件名统一为 **AGENTS.md**（非 README.md；README.md 为人类导航，仅重定向至 AGENTS.md）
- Claude Code 会自动注入 `pkg/<X>/AGENTS.md`；Gemini / GPT / Cursor 等工具**需手动读取**

**arch ↔ specs 分工**：
- `arch/` = 系统**是什么**（设计）：M_X 实现 / ARCH SSoT 锚点 / 00-Dict 概念 / state.yaml 阈值
- `arch/decisions/` = 决策档案（why-not 单源）：ADR 是"反复被驳的方案"档案，与 M_X 是引用关系
- `specs/` = AI 代码**怎么写**（规范）：R1~R8 反模式 + R2 命名 SSoT + 工作流 + 审查清单

## 当前阶段

代码开发，覆盖全仓库。规约明确的模块优先开发；规约缺失/模糊 → 编码前补设计。
