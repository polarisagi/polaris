# polaris

> 开源自托管 AI Agent | Go 1.26+ + Rust 1.94+ | 28 internal module / 4 layer | 最低 2GB VPS 可运行，Tier 0 (8GB) 为开发推荐地板 | provider-agnostic (`configs/defaults.toml` 推荐 DeepSeek V4)

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

**[HE-Rules]** 收敛于 `docs/arch/00-Global-Dictionary.md`，完整工程化实现要求见 `docs/specs/00-Constitution.md §R3` + `docs/specs/09-LLM-Agent-Production.md`：

| # | 不变量 | 核心禁止 |
|---|--------|----------|
| HE-1 | **可观测优先** — 每条路径必有 OTel span + Prometheus 埋点 | 能算不上报、无埋点的中间状态 |
| HE-2 | **可验证执行** — 安全边界必须物理/密码学可验证 | 概率过滤当安全边界、Taint 静默丢失、nil 安全门放行 |
| HE-3 | **可组合原语** — 接口在调用方定义，跨模块用结构化事件 | service 调 DAO、字符串隐式耦合、工具注册旁路 ExecuteTool |
| HE-4 | **数据驱动迭代** — Eval Harness 驱动，所有变更需 CI 门控 | 跳过 Eval 直改阈值 / Prompt / 权重 |
| HE-5 | **状态机持控制流** — Go FSM 主导；LLM 是协处理器；禁 `while True: call LLM` | LLM 回同内容直接驱动流程跳转、FSM 锁内做 IO |
| HE-6 | **State-in-DB** — 持久化落盘，跨模块走异步事件 | 状态仅内存、DB 连接期间发起 LLM 调用（R1.16） |
| HE-7 | **防退化边界** — 守住核心体系 (五防线与 Memory-Write-Tool) | 绕过 ExecuteTool 写记忆、弱化 Taint/Cedar/KillSwitch/SSRFGuard |

**[Tier-0]** 核心路径（含 SurrealDB kv-mem + Embedding + STT + Wasm 沙箱）必须在 2GB+ VPS 可运行；8GB 为推荐开发地板（Tier0），本地推理需 Tier1（16GB+）。超限能力走硬件门控解锁，不得作硬依赖。

## 项目结构

```text
api/proto/                   Protobuf 原始定义
cmd/polaris/                 主入口 (尽量保持极简，将初始化逻辑下推到 internal/cli)
configs/                     嵌入式启动配置（随二进制打包）
  threshold-examples/        阈值覆盖示例（m*.toml）
  agents/ prompts/ *.yaml    各类启动配置

internal/
  # --- L1 认知/执行层 ---
  agent/           核心状态机 (FSM)、生命周期、思考循环
    fsm/           状态机实现（state_machine / transitions / epoch）
    context/       感知上下文（memory_context / pii_vault / whisper / persona_refiner）
  action/          动作执行层（CodeAct / LAM / Hook / 能力令牌）
    codeact/       即时代码执行
    lam/           LAM 动作执行
    hook/          Hook 框架（RunScript 脚本执行）
  memory/          记忆系统（Working / Episodic / Semantic / Procedural）
    consolidation/ 记忆巩固 Worker（含 SemanticCompressHandler VFS 语义压缩）
    graph/         世界模型图 + 突触可塑性
    retrieval/     HybridRetriever 多路融合检索
    store/         记忆物理存储后端
  tool/            工具注册与执行（InMemoryToolRegistry + PolicyGate 五阶段）
    builtin/       内置工具集（每工具独立子目录含 tool.yaml/schema.json）
    sandbox/       工具沙箱执行适配层
  sandbox/         沙箱执行环境（Wasm / 容器三级回退）
  prompt/          提示词模板管理
    optimizer/     提示词优化器
  vfs/             虚拟工作区与文件系统隔离

  # --- 单/多 Agent 执行引擎层（2026-07-12 新增，服务 L1 + L2）---
  execute/         执行引擎：只负责"如何跑完一份已确定的计划/图"，不做决策
                   （详见 internal/execute/CLAUDE.md、ADR-0046）
    dag/           单 Agent 内工具链 DAG 执行器 + S_VALIDATE 四层校验管线
                   （原 agent/dag，经 agent/provider.go DAGRunner/DAGValidator
                   消费端接口反向注入 internal/agent）
    orchestrator/  Blackboard + Worker + 多模式编排（Sequential/Parallel/
                   MapReduce/PatternDAG/StateGraph/CSV-Fanout；原
                   swarm/orchestrator，经 internal/swarm 消费）

  # --- L2 协同/知识层 ---
  swarm/           多 Agent 协同策略（任务分解/拓扑路由/Supervisor Tree）
    planner/       任务规划与分解
    supervisor/    Supervisor Tree
    topology/      三角色默认拓扑（Supervisor/Librarian/Governance）
    agents/        常驻 goroutine Agent（governance/security_audit/memory）
  learning/        自进化引擎（三环架构）
    surprise/      SurpriseIndex + 漂移检测
    reflexion/     HER ReflexionEngine + 异步耳语通道
    synthetic/     合成评估用例 + Python Skill 生成（Logic Collapse 蒸馏）
    curriculum/    自动课程 + 动态难度校准
  knowledge/       RAG + 知识图谱
    graphrag/      图谱构建管线、图遍历、社区摘要
    connector/     外部知识源（Obsidian / 同步调度 / ExtensionLibrarianHandler 扩展索引）
  extension/       扩展注册与运行时
    mcp/           MCP 客户端管理（LoadFromDB / TaintPreservingDecoder）
    plugin/        插件系统
    skill/         Skill 编译与执行（LogicCollapse Python / ContainerSandbox）
    marketplace/   插件市场（安装/卸载/级联删除）
    native/        本地扩展激活器
    models/        扩展模型定义

  # --- L3 接口/治理层 ---
  gateway/         HTTP API 网关（REST/SSE/OpenAI 兼容）
    server/        核心 HTTP 服务（middleware / logstream）
      chat/        聊天接口处理
      plugin/      插件管理接口
      provider/    Provider 管理接口
      sysadmin/    系统管理接口（channels/MCP/sysinfo 等）
    egress/        出口网关
    authcontext/   认证上下文
    types/         共享类型
  automation/      定时调度与自动化工作流
    hitl/          HITL 人工审批网关（ESCALATE 协议）
  eval/            评估与 Benchmark 引擎
    harness/       评估执行器（EvalCase / RunnerImpl / SQLiteEvalStore）
    analysis/      元评估 / 采样监控 / 影子执行 (ShadowExecutor)
    control/       访问控制（RBAC/PBAC）
  channel/         聊天平台双向适配器（TG/Discord）
    adapter/       各平台实现
  sysmgr/          系统资源管理（downloader/sysinfo 已迁出至 L0，见下）
    updater/       自动更新
    locale/        本地化
  cli/             命令行引导与命令处理

  # --- L0 基础设施层 ---
  store/           存储机制
    repo/          SQLite Repository 实现层（对应 protocol/repo/ 接口）
    search/        全文/语义检索（BM25）
    audit/         事件日志与决策日志
  observability/   监控度量与遥测
    metrics/       Prometheus 指标 + instruments（TokenBurnRate CANONICAL）
    probe/         硬件探针 / 内存探针 / Tier 参数 / FeatureGate
    trace/         链路追踪 / LLM 调用埋点
  security/        安全体系
    taint/         五级污点传播系统（TaintedString / SafeString）
    policy/        Cedar 策略引擎（三层防线 deny-by-default）
    token/         能力令牌（Ed25519 签名）
    network/       网络隔离（SafeDialer / LocalOnly）
    guard/         Factuality Guard / PII 检测
  llm/             大模型对接层
    adapter/       各 Provider 适配器（anthropic/deepseek/google/ollama/openai）
    stt/           语音识别（Sherpa-ONNX）
    tts/           语音合成
  ffi/             Rust dylib 零 CGO 高性能桥接（purego）
  sysinfo/         系统信息采集（硬件探针，供 agent 硬件分级 / sys_probe 工具使用）
  downloader/      通用资源下载（HTTP/Git + 系统代理探测，模型二进制/插件包下载共用）
  # 2026-07-07 从 sysmgr/ 物理迁移至此：原归类在 L3 但被 L0(llm/ollamamgr、
  # llm/stt、llm/tts)/L1(agent、tool/builtin/sys_probe)/L2(extension/marketplace)
  # 广泛引用，属于分类与实际用途不匹配（不含 L3 接口治理语义的通用工具）。

  # --- 通用契约（所有层均可引用）---
  protocol/        跨模块共享类型 + 接口契约 + DDL
    repo/          Repository 接口定义（对应 store/repo/ 实现）
    pb/            Protobuf 生成文件
    schema/        DDL SQL 文件（31 个，SSoT）
  config/          配置加载 + 编译期不变量
  lint/            CI 静态扫描规则
  bootstrap/       模块生命周期统一编排（Bootable + DependencyMap + Kahn 拓扑排序，四阶优雅关停）

pkg/               通用工具（无业务逻辑，任意层可引用）
  apperr/          统一错误类型（禁裸 error 泄漏调用链）——`apperr.New/Wrap/IsCode/HTTPStatus`
  types/           基础共享类型
  version/         版本信息

rust/substrate/   R**docs/arch/decisions/ 文件清单**（ADR-0001~0050，0032 未分配，按需 grep 主题词）：
- 0001 观测单例 · 0002 Skill 注册合并 · 0003 SQLite modernc · 0004 Tier-0 硬件层 · 0005 purego FFI Cedar
- 0030 Tier2 Semantic Embedding · 0031 TTS 三路提供商
- 0006 state.yaml SSoT · 0007 污点五级 · 0008 沙筆三级回退 · 0009 KillSwitch 三阶段 · 0010 SurrealDB 认知存储
- 0011 CGO→purego（含 Tree-sitter 受限例外）· 0012 spec 一致性测试 · 0013 Lint 阶段1 · 0014 对抗审查 Action · 0015 Codex 特性集成
- 0016 统一信任扩展模型 · 0017 MCP Streamable HTTP · 0018 MCP 污点解码器 · 0019 扩展实例统一安装表
- 0020 DeepSeek V4 默认提供商 · 0021 核心机制实现（SurpriseIndex/WasmTester/BM25/FSM）
- 0022 ThinkingMode 三档路由 · 0023 episodic 写路径双轨制 · 0024 GovernanceAgent 代码安全三层防线
- 0025 全局架构审查修复（R21）· 0026 Logic Collapse Python+ContainerSandbox 运行时
- 0027 已废弃→ADR-0025 · 0028 已废弃→ADR-0025
- 0029 Phase 1-2 系统加固（AgentPool / VFS 墓碑 / SQL Fitness / SafeGo 全量 / OS Fault 注入 / ShadowExecutor）
- 0033 记忆子系统范围限制与扩展（Won't Do + ZoneCoreMemory + 时序检索）· 0034 已废弃→ADR-0011
- 0035 已废弃→ADR-0033 · 0036 已废弃→ADR-0033 · 0037 Pattern DAG 编排
- 0038 已废弃→ADR-0029 · 0039 Gateway 控制权移交 FSM（废除 MVP 直通模式）
- 0040 已废弃→ADR-0041 · 0041 StateGraphExecutor 显式状态图编排 · 0042 HITL AskUser 咨询闭环
- 0043 Generative UI SSE 集成 · 0044 M7 模块边界拆分暂缓 · 0045 保留五级污点传播 · 0046 execute 模块化（单/多 Agent 执行引擎收敛）
- 0047 taint_sanitizer 二级降级接入 S_VALIDATE（复用 ExemptionVault）· 0048 ContinuousSamplingMonitor 生产流量 1% LLM Judge 采样 · 0049 sCtx.SessionID 根因 Bug 修复
- 0050 删除中心化 Orchestrator/Worker/内存 Blackboard 与 SwarmRouter/CapabilityRegistry/TopologyEvolverService（自订阅 CAS 认领胜出）
- 错误统一 `pkg/apperr`（`apperr.New/Wrap`；禁裸 `errors.New`/`fmt.Errorf` 泄漏调用链）
- `internal/` 禁全局可变变量（并发安全 + 测试隔离；ADR-0001 豁免仅限 observability/metrics 一等公民指标）
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
- `docs/specs/05-Coding-Workflow.md` — Spec-First 四阶段工作流（含 W0 故障排查工作流）
- `docs/specs/CHANGELOG.md` — 扫近 5 条规范变更（确认无破坏性改动）

**排障场景**（非新功能开发，先看这里）：`docs/arch/INDEX.md §0 症状索引` 命中即跳对应文件，未命中走 `docs/specs/05-Coding-Workflow.md W0`；定位到新根因后强制回填一行到 §0，成本一行以内。

**编码前装载**（按场景挑 1~3，禁止全量预读）：
1. `docs/arch/INDEX.md` → §2 场景表选 1~3 个 `M_X`，按文件头 §偏移跳读精读章节
2. `docs/arch/00-Global-Dictionary.md` → `[Concept]` 唯一权威源 + XR-01~07 跨模块规则
3. `docs/arch/ARCHITECTURE.md` → SSoT 锁点；仅 Staging 7 阶段 / HT0 预算 / 变更控制 / 配置层 4 场景必读
4. `docs/arch/decisions/ADR-XXXX-*.md` → 已驳方案档案（ADR-0001~0050，0032 未分配）；**"为什么不用 X" 先 grep 这里**，避免重提已驳方案
5. `docs/arch/spec/state.yaml` → 状态机 + 全模块阈值 SSoT，按 `§par/§staging/§taint/...` 偏移局部读
6. `docs/specs/0X-*.md` → 按域选读：Go↑01 / Rust↑02 / Agent↑03 / 跨模块↑04 / 审查↑06 / 提交前↑06
7. `docs/specs/07-Reference-Implementation.md` → 写新代码前定位 canonical 标瑯
8. `docs/specs/09-LLM-Agent-Production.md` → **写任何 Agent/LLM/Tool/RAG/Memory 相关代码前必读**（A-01~A-14 陷阱 + P-1~P-9 生产原则 + RAG/并发安全检查清单）
9. `internal/protocol/` → 跨模块共享类型与接口契约
10. `internal/protocol/schema/NNN_*.sql` → **DDL Schema SSoT**（001~024 + 028~034，共 31 个 SQL 文件，025~027 保留未用）；修改 Schema 前必读目标表文件，禁 ALTER TABLE 补丁（上线前直接改原始文件 + 删库重建）

**docs/arch/decisions/ 文件清单**（ADR-0001~0050，0032 未分配，按需 grep 主题词）：
- 0001 观测单例 · 0002 Skill 注册合并 · 0003 SQLite modernc · 0004 Tier-0 硬件层 · 0005 purego FFI Cedar
- 0030 Tier2 Semantic Embedding · 0031 TTS 三路提供商
- 0006 state.yaml SSoT · 0007 污点五级 · 0008 沙箱三级回退 · 0009 KillSwitch 三阶段 · 0010 SurrealDB 认知存储
- 0011 CGO→purego · 0012 spec 一致性测试 · 0013 Lint 阶段1 · 0014 对抗审查 Action · 0015 Codex 特性集成
- 0016 统一信任扩展模型 · 0017 MCP Streamable HTTP · 0018 MCP 污点解码器 · 0019 扩展实例统一安装表
- 0020 DeepSeek V4 默认提供商 · 0021 核心机制实现（SurpriseIndex/WasmTester/BM25/FSM）
- 0022 ThinkingMode 三档路由 · 0023 episodic 写路径双轨制 · 0024 GovernanceAgent 代码安全三层防线
- 0025 全局架构审查修复（R21）· 0026 Logic Collapse Python+ContainerSandbox 运行时
- 0027 Gemini 执行后遗留实现缺口修复（BUG-1~4：LAM接入/CC-2零值/SafeGo/taint读路径）
- 0028 Phase 0 P0 Bug 修复（Scheduler 内稳态防抖 / FSM SafeGo / Cedar SafeGo / SurpriseCalculator 接入）
- 0029 Phase 1-2 系统加固（AgentPool / VFS 墓碑 / SQL Fitness / SafeGo 全量 / OS Fault 注入）
- 0033 记忆子系统范围限制 · 0034 Tree-sitter CGO 例外授权
- 0035 已废弃→ADR-0033 · 0036 已废弃→ADR-0033 · 0037 Pattern DAG 编排
- 0038 已废弃→ADR-0029 · 0039 Gateway 控制权移交 FSM · 0040 已废弃→ADR-0041
- 0041 StateGraphExecutor 显式状态图编排 · 0042 HITL AskUser 咨询闭环
- 0043 Generative UI SSE 集成 · 0044 M7 模块边界拆分暂缓 · 0045 保留五级污点传播 · 0046 execute 模块化（单/多 Agent 执行引擎收敛）
- 0047 taint_sanitizer 二级降级接入 S_VALIDATE（复用 ExemptionVault）· 0048 ContinuousSamplingMonitor 生产流量 1% LLM Judge 采样 · 0049 sCtx.SessionID 根因 Bug 修复
- 0050 删除中心化 Orchestrator/Worker/内存 Blackboard 与 SwarmRouter/CapabilityRegistry/TopologyEvolverService（自订阅 CAS 认领胜出）

**internal/protocol/schema/ DDL 清单**（修改 Schema 前按需加载对应文件，31 个 SQL 文件；025~027 编号段**刻意预留**——对应表已被重构合并至其他表，编号不复用防历史混淆；`embed.go` 使用 `//go:embed *.sql` 自动包含所有实际 .sql 文件，跳号不影响编译）：
```
001_events · 002_outbox · 003_episodic_memory · 004_semantic_memory · 005_workspace_vfs
006_decision_log · 007_tasks · 008_skills · 009_rag_chunks · 010_self_improve
011_providers · 012_channels · 013_chat · 014_cron_jobs · 015_mcp_servers
016_preferences · 017_automations · 018_plugin_marketplaces · 019_extension_catalog · 020_extension_instances · 021_plugins
022_provider_catalog · 023_notes · 024_reflection_memory · 028_apps · 029_workflows
030_oom_guard_log · 031_planner_sessions · 032_mock_response_cache · 033_model_version_registry · 034_core_memory
```

**禁止**：
- 未读 INDEX 直接加载多个 M_X
- 将 `ROADMAP.md` `DIAGRAMS.md` 列为默认加载（人类参考层，按需 §跳读）
- 将 `ARCHITECTURE.md` 全量预读（SSoT 锚点，按场景按 §跳读）
- 以 ALTER TABLE / ADD COLUMN 补丁文件修改 Schema（上线前直接改原始 SQL 文件）

**模块上下文（重要）**：进入 `internal/<X>/` 时，若该目录存在 `internal/<X>/CLAUDE.md` 则必读（当前仅 action/agent/extension/learning/memory/swarm 6 个模块有对应文件，其余模块不存在时不适用本条）。
- 各包规范文件名统一为 **CLAUDE.md**（Claude Code 原生自动注入子目录 CLAUDE.md；Gemini / GPT / Cursor 等工具**需手动读取**）
- README.md 为人类导航页，仅重定向至 CLAUDE.md，不含规范内容

**arch ↔ specs 分工**：
- `arch/` = 系统**是什么**（设计）：M_X 实现 / ARCH SSoT 锚点 / 00-Dict 概念 / state.yaml 阈值
- `arch/decisions/` = 决策档案（why-not 单源）：ADR 是"反复被驳的方案"档案，与 M_X 是引用关系
- `specs/` = AI 代码**怎么写**（规范）：R1~R8 反模式 + R2 命名 SSoT + 工作流 + 审查清单

## 当前阶段

代码开发，覆盖全仓库。规约明确的模块优先开发；规约缺失/模糊 → 编码前补设计。
