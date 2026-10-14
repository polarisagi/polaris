# polaris

> 开源自托管 AI Agent | Go 1.26+ + Rust 1.94+ | 28 internal module / 4 layer | 主形态桌面端 + 浏览器，同一守护进程亦可部署 VPS（核心路径最低 2GB），Tier 0 (8GB) 为开发推荐地板 | provider-agnostic (`configs/defaults.toml` 推荐 DeepSeek V4)
>
> **本文件及全部 `docs/` 的读者定位：AI 优先**（ADR-0081，原 0092）。人类可读性不是维护目标；`§跳读` 行、`[HE-Rule-x]`/`[Concept]` 标签、`inv_` 编号表、紧凑表格均为一等公民导航结构，不得以"人类阅读噪音"为由删除。

## 角色与交互纪律

资深系统架构师 + 底层工程师。域：Go 并发、Rust FFI 安全边界、嵌入式 DB 选型、AI Agent 认知架构、Harness Engineering。

- **[强制] 中文输出**（分析/讨论/文档/决策）；直接落盘
- 结论前置，无寒暄与铺垫（ADR-0112）
- 只交付当前目标的最少代码集；禁止超前抽象、臆测开发、顺手重构未损坏内容、擅改历史排版（100% 指令溯源）
- 指令歧义或架构冲突 → 主动提问，禁止静默决策
- 结论须溯源到**代码事实**或**规范性文档条款**（文件名 + 章节）；描述性文档与代码不符 → 以代码为准并订正（见 `§文档可修订性`）
- 语言：代码注释中文且只写"为什么"（R5）；标识符英文；提交信息 `<type>(<scope>): <中文述>`，scope=包名

## 不变量

**[HE-Rules]** 收敛于 `docs/arch/00-Global-Dictionary.md`，工程化实现见 `docs/specs/00-Constitution.md §R3` + `09-LLM-Agent-Production.md`：

| # | 不变量 | 核心禁止 |
|---|--------|----------|
| HE-1 | **可观测优先** — 每条路径必有 OTel span + Prometheus 埋点 | 能算不上报、无埋点的中间状态 |
| HE-2 | **可验证执行** — 安全边界必须物理/密码学可验证 | 概率过滤当安全边界、Taint 静默丢失、nil 安全门放行 |
| HE-3 | **可组合原语** — 接口在调用方定义，跨模块用结构化事件 | service 调 DAO、字符串隐式耦合、工具注册旁路 ExecuteTool |
| HE-4 | **数据驱动迭代** — Eval Harness 驱动，所有变更需 CI 门控 | 跳过 Eval 直改阈值 / Prompt / 权重 |
| HE-5 | **状态机持控制流** — Go FSM 主导；LLM 是协处理器 | `while True: call LLM`、LLM 回同内容直接驱动跳转、FSM 锁内做 IO |
| HE-6 | **State-in-DB** — 持久化落盘，跨模块走异步事件 | 状态仅内存、DB 连接期间发起 LLM 调用（R1.16） |
| HE-7 | **防退化边界** — 守住五防线与 Memory-Write-Tool | 绕过 ExecuteTool 写记忆、弱化 Taint/Cedar/KillSwitch/SSRFGuard |

**[Tier-0]** 核心路径（SurrealDB kv-mem + Embedding + Wasm 沙箱）必须在 2GB+ VPS 可运行；8GB 为推荐开发地板（Tier0），本地推理需 Tier1（16GB+）；超限能力走硬件门控解锁，不得作硬依赖（R4）。语音按总内存/核数分三档（A 全关 / B 仅 STT / C STT + 服务端 TTS，TTS 另需 RTF ≤ 0.8 基准），阈值与资产状态机见 ADR-0110。麦克风需安全上下文（localhost 可用，远程 VPS 须 HTTPS）。

## 项目结构

```text
api/proto/       Protobuf 原始定义
cmd/polaris/     主入口（装配落点 boot_*.go，手工装配；CLI 客户端 cli*.go 一律走 HTTP，不直连内核与数据库，ADR-0096）
configs/         嵌入式启动配置；threshold-examples/ 阈值覆盖示例（m*.toml）

internal/        28 模块 / 4 层。★ = 该目录有 CLAUDE.md，进入时必读，子包细节以其为准
  # L1 认知/执行层
  agent/ ★     FSM、生命周期、思考循环、感知上下文（fsm / context / schemavalidate）
  action/ ★    动作执行（codeact / lam / hook；能力令牌）
  memory/ ★    四层记忆（consolidation / graph / retrieval / store / compact）
  tool/          工具注册与执行（PolicyGate 五阶段；builtin/ catalog/ dispatch/ sandbox/）
  sandbox/ prompt/ vfs/   沙箱环境（Wasm/容器三级回退）/ 提示词模板 / 虚拟工作区
  execute/ ★   执行引擎（ADR-0046）：dag/ 工具链 DAG + S_VALIDATE；orchestrator/ Blackboard + 多模式编排
  # L2 协同/知识层
  swarm/ ★     多 Agent 协同策略（planner / supervisor / agents）
  learning/ ★  自进化三环（surprise / reflexion / synthetic / curriculum）
  knowledge/     RAG + 知识图谱（graphrag/ connector/）
  extension/ ★ 扩展注册与运行时（mcp / marketplace / lifecycle / pluginspec / skill / native）
  # L3 接口/治理层
  gateway/       HTTP 网关；server/ ★ 核心服务；session/ 会话编排（零 net/http，ADR-0066）；mesh/ egress/ authcontext/ elicitation/ httputil/ types/
  automation/    定时调度与工作流（hitl/ 审批网关、notify/）
  eval/          评估引擎（harness / analysis / benchmark / control / regression）
  channel/       聊天平台适配器 TG/Discord
  sysmgr/        系统管理（updater / locale / osutils）
  # L0 基础设施层
  store/         SQLite/SurrealDB 适配、MutationBus、repo/ search/ audit/
  observability/ metrics/ probe/（硬件探针 + Tier + FeatureGate）trace/ budget/
  security/      taint/ policy/（Cedar 三层）token/ network/（SafeDialer）guard/ credential/ classifier/
  llm/           adapter/ stt/ tts/ audiorun/ audioassets/ embedonnx/ modelregistry/ safecall/
  ffi/ sysinfo/ downloader/ runtimeinfo/   Rust 桥接（purego）/ 硬件采集 / 通用下载 / 运行时状态（run/，ADR-0096）
  # 通用契约（所有层可引用）
  protocol/      跨模块类型与接口契约；repo/ pb/ schema/（DDL SQL（45 个，SSoT））
  config/        配置加载 + 编译期不变量 + 系统参数单一事实源 (system_params.go)
  lint/          CI 静态扫描规则

pkg/             无业务逻辑通用工具：apperr/ concurrent/ graph/ offpeak/ types/ util/ version/
desktop/         桌面外壳（Tauri v2，ADR-0096）：只做进程/窗口/托盘/通知，禁止业务语义的 Tauri command——客户端与内核之间只有 HTTP/SSE
rust/substrate/  Rust FFI 库（Cedar + SurrealDB-Core，purego，ADR-0011）
```

## 编码约定

错误统一 `pkg/apperr`（R1.2）、`internal/` 禁全局可变变量（R1.3，ADR-0001 豁免）、跨模块走 `internal/protocol/` 结构化事件（R1.6）、Rust 仅性能关键 FFI——全文见 `docs/specs/00-Constitution.md`。

- **[强制] 提交前自检**：`git commit` 前先 `make lint`（或 `make fmt && make lint`）全绿。改动涉及 `docs/arch/` 或包路径迁移，追加 `make docs-refs`（ADR-0081；白名单 `tools/baselines/docs-refs-allowlist.txt` 仅收历史注记）。
- **[强制] 新增/修改门控**：`tools/*_lint.go`、`tools/*_check.go` 必须在 `tools/lint-selftest.txt` 登记负向用例并使 `make lint-selftest` 通过。未经负向验证的规则不算 landed，判定由门控做，不接受自述。
- **[强制] 配置变更**：改 `internal/config/` 结构体后必须 `make gen-threshold-examples` 并提交 TOML。
- **[强制] 音频外部坐标**：改 `internal/llm/stt|tts` 下载 URL/资产名、`configs/defaults.toml [inference.stt|tts]` 模型 URL、`internal/llm/audioassets` 清单后，本机跑 `make audio-nettest`（依赖外网，不进 CI；ADR-0110）。升级 sherpa-onnx 须先重测 FFI 偏移并改 `stt.SherpaABIVersion`。
- **[强制] DDL 修改策略**：`internal/protocol/schema/NNN_*.sql` 是 Schema SSoT，禁止 ALTER TABLE / ADD COLUMN 补丁文件（门控：`Test_inv_NoAlterTableInSchema`）。流程见 `docs/specs/05-Coding-Workflow.md W7`。
  - **上线前**（`§当前阶段` 未标注"上线后"）：直接修改原始建表文件；开发库删除重建（`rm ~/.polaris/data/polaris.db`）。
  - **上线后**：新增编号迁移文件，不得修改已应用历史文件。
  - Phase 判断 SSoT：本文 `§当前阶段`；不确定 → 主动提问。
- **[强制] 不可逆操作确认**：`git push`、删除开发库/数据目录、发布签名与密钥操作、删除已跟踪文件（计划内合并删除除外）、改写 git 历史——执行前须经用户确认（ADR-0112）。
- **[强制] Git 署名**：所有提交统一署名 `MrLaoLiAI <polarisagi.online@gmail.com>`。**绝对禁止** commit message 含 `Co-authored-by: Claude/Gemini ...` 或任何 AI 工具联合署名（污染 Contributors 列表）；代理自动生成的须在 `git commit` 前剔除。

## 文档加载协议

> `docs/arch` + `docs/specs` 全量 ≈ 470K token（字节/2.2，与 `tools/docs_gen.go` est_tok 同口径），必爆。**默认按需加载**，不预读 M_X.md。

**会话启动必读**（不含本文件；按需加载，不预读 M_X）：
- `docs/specs/INDEX.md` — 规范导航入口
- `docs/specs/00-Constitution.md` — 反模式 R1~R8 + 命名 SSoT R2 + HE-Rules 量表
- `docs/specs/05-Coding-Workflow.md` — Spec-First 四阶段（含 W0 排障流程）
- `docs/specs/CHANGELOG.md` — 只扫头部近 5 条

**排障场景**：先看 `docs/arch/INDEX.md §0 症状索引`，命中即跳；未命中走 `05-Coding-Workflow.md W0`；定位到新根因后强制回填一行到 §0。

**编码前装载**（按场景挑 1~3，禁止全量预读）：
1. `docs/arch/INDEX.md` §2 场景表选 1~3 个 `M_X`，按文件头 §跳读 行号精读
2. `docs/arch/00-Global-Dictionary.md` — `[Concept]` 唯一权威源 + XR-01~13 跨模块规则
3. `docs/arch/ARCHITECTURE.md` — SSoT 锁点；仅 Staging 7 阶段 / HT0 预算 / 变更控制 / 配置层 4 场景必读
4. `docs/arch/decisions/ADR-XXXX-*.md` — 决策档案（决策 + 后果边界 + 反例守护）。**"为什么不用 X" 先 grep 这里**；重提已驳方案须带新事实/新约束并在原 ADR 追记复核。编号索引与「已删除」编号对照表 SSoT = `docs/arch/decisions/README.md`，本文不维护副本
5. `docs/arch/spec/state.yaml` — 状态机 + 全模块阈值 SSoT，按 `§par/§staging/§taint/...` 局部读
6. `docs/specs/0X-*.md` — 按域选读（见 INDEX 加载策略表）；写新代码前读 `07-Reference-Implementation.md` 标杆；写任何 Agent/LLM/Tool/RAG/Memory 代码前必读 `09-LLM-Agent-Production.md`
7. `internal/protocol/` — 跨模块类型与接口契约
8. `internal/protocol/schema/NNN_*.sql` — DDL SSoT（001~024 + 029~049，共 45 个 SQL 文件；014 规划删除预留）；`ls internal/protocol/schema/*.sql` 即得（45 个）。014、025~028 刻意预留不复用（014 随 ADR-0114 废弃删除预留，025~027 已合并重构，028 apps 随 ADR-0103 删除），不得报为"缺失"。修改前必读目标表文件

**禁止**：未读 INDEX 直接加载多个 M_X；默认加载 `ROADMAP.md` `DIAGRAMS.md`（人类参考层）；全量预读 `ARCHITECTURE.md`。

**多代理协作**：多步骤/跨会话任务或设计与实现分属不同模型/子代理时，按 `docs/specs/05-Coding-Workflow.md W8` 执行（计划文件 `local_playground/<任务>/PLAN.md`、文件所有权、汇报须核实、冲突只记录不改）。

**模块上下文**：进入 `internal/<X>/` 时，若存在 `internal/<X>/CLAUDE.md` 则必读（当前 action/agent/execute/extension/learning/memory/swarm 及 gateway/server 共 8 个目录有对应文件）。Claude Code 自动注入子目录 CLAUDE.md，其他 AI 工具需手动读取。

**arch ↔ specs 分工**：`arch/` = 系统是什么（M_X 实现、SSoT 锚点、概念字典、state.yaml 阈值）；`arch/decisions/` = 决策档案（why-not 单源，与 M_X 为引用关系）；`specs/` = AI 代码怎么写（R1~R8 + 工作流 + 审查清单）。

## 文档可修订性

文档分两类，冲突时判定方向**相反**：

| 类 | 范围 | 与代码冲突时 |
|---|---|---|
| **描述性**（记录代码事实） | `arch/ARCHITECTURE.md` 现状章节、`arch/M_X.md` 实现描述、ADR 的事实引用（行数/路径/调用点/接线状态） | **以代码为准**，同步订正文档 |
| **规范性**（约束代码行为） | `§不变量` HE-1~7、`specs/` R1~R8 + R2 命名、`arch/spec/state.yaml` 阈值、`arch/00-Global-Dictionary.md` 概念、`protocol/schema/` DDL、ADR 的决策结论与「重新评估触发条件」 | **以文档为准**，改代码 |

判定归属：该段被推翻后，是文档失真，还是代码失控？前者描述性，后者规范性。同段兼具 → 按规范性处置。

规范性条款可推翻，但**必须先改文档再改代码**，且带门控证据（Eval / lint / 可复现用例）。先落代码再补文档等同用既成事实降级不变量（HE-7 要拦的）。`§当前阶段` 为开发期：存在全局更优解时**应当**修订，不为迁就旧文档写次优代码。

修订方式：在原节**追记**复核日期 + 推翻理由，原结论保持可见；禁止静默改写使旧决策看起来从未存在。`decisions/` 免 `make docs-refs` 扫描，但免扫 ≠ 免改（ADR-0081）。

**三项不可变**：ADR 编号不重排/不复用（理由见 `decisions/README.md`）；上线后已应用的 DDL 迁移文件；他人 commit 历史。

## 当前阶段

代码开发，覆盖全仓库。规约明确的模块优先开发；规约缺失/模糊 → 编码前补设计。
