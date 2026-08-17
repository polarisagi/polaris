# polaris

> 开源自托管 AI Agent | Go 1.26+ + Rust 1.94+ | 29 internal module / 4 layer | 最低 2GB VPS 可运行，Tier 0 (8GB) 为开发推荐地板 | provider-agnostic (`configs/defaults.toml` 推荐 DeepSeek V4)
>
> **本文件及全部 `docs/` 的读者定位：AI 优先**（2026-08-09 裁决）。人类可读性不是维护目标；`§跳读` 行、`[HE-Rule-x]`/`[Concept]` 标签、`inv_` 编号表、紧凑表格均为一等公民导航结构，不得以"人类阅读噪音"为由删除。

## 角色

资深系统架构师 + 底层工程师。域：Go 并发、Rust FFI 安全边界、嵌入式 DB 选型、AI Agent 认知架构、Harness Engineering。

## 交互纪律

- **[强制] 中文输出**（分析/讨论/文档/决策）
- 直接落盘，禁止问候/解释/确认语/Markdown 包裹
- **[Token 效率]** 结论前置，依据紧随。禁止描述性铺垫、拟人化、情感确认、修饰词
- 只交付当前目标的最少代码集。禁止超前抽象、臆测开发
- 100% 指令溯源。禁止顺手重构未损坏内容、擅改历史排版
- 指令歧义或架构冲突 → 主动提问，禁止静默决策
- 结论须溯源到**代码事实**或**规范性文档条款**，引用指明文件名 + 章节/段落；描述性文档与代码不符 → 以代码为准并同步订正（两类之分见 `§文档可修订性`）

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
api/proto/       Protobuf 原始定义
cmd/polaris/     主入口（实际装配落点为 boot_*.go，维持手工装配，internal/cli 为未接线的 CLI 引导契约）
configs/         嵌入式启动配置（随二进制打包）；threshold-examples/ 阈值覆盖示例（m*.toml）

internal/        29 模块 / 4 层。★ = 该目录有 CLAUDE.md，进入时必读，子包细节以其为准不在此重复
  # --- L1 认知/执行层 ---
  agent/       ★ 核心状态机 (FSM)、生命周期、思考循环、感知上下文（fsm / context）
  action/      ★ 动作执行层（codeact / lam / hook；能力令牌）
  memory/      ★ 四层记忆 Working/Episodic/Semantic/Procedural（consolidation / graph / retrieval / store）
  tool/          工具注册与执行（InMemoryToolRegistry + PolicyGate 五阶段）
    builtin/     内置工具集（每工具独立子目录含 tool.yaml/schema.json）
    sandbox/     工具沙箱执行适配层
  sandbox/       沙箱执行环境（Wasm / 容器三级回退）
  prompt/        提示词模板管理（optimizer/ 提示词优化器）
  vfs/           虚拟工作区与文件系统隔离

  # --- 单/多 Agent 执行引擎层（服务 L1 + L2；ADR-0046）---
  execute/     ★ 只负责"如何跑完一份已确定的计划/图"，不做决策
               dag/ 工具链 DAG + S_VALIDATE 四层校验；orchestrator/ Blackboard + 多模式编排

  # --- L2 协同/知识层 ---
  swarm/       ★ 多 Agent 协同策略（planner / supervisor / topology / agents 常驻 goroutine）
  learning/    ★ 自进化引擎三环（surprise / reflexion / synthetic / curriculum）
  knowledge/     RAG + 知识图谱
    graphrag/    图谱构建管线、图遍历、社区摘要
    connector/   外部知识源（Obsidian / 同步调度 / 扩展索引）
  extension/   ★ 扩展注册与运行时（mcp / plugin / skill / marketplace / native / models）

  # --- L3 接口/治理层 ---
  gateway/       HTTP API 网关（REST/SSE/OpenAI 兼容）
    session/     SessionOrchestrator 会话生命周期编排（零 net/http 依赖，ADR-0085）
    server/    ★ 核心 HTTP 服务（middleware / logstream / chat / plugin / provider / sysadmin）
    egress/ authcontext/ types/   出口网关 / 认证上下文 / 共享类型
  automation/    定时调度与自动化工作流（hitl/ HITL 审批网关，ESCALATE 协议）
  eval/          评估与 Benchmark 引擎
    harness/     评估执行器（EvalCase / RunnerImpl / SQLiteEvalStore）
    analysis/    元评估 / 采样监控 / 影子执行（ShadowExecutor）
    control/     访问控制（RBAC/PBAC）
  channel/       聊天平台双向适配器 TG/Discord（adapter/ 各平台实现）
  sysmgr/        系统资源管理（updater/ 自动更新，locale/ 本地化）
  cli/           命令行引导与命令处理

  # --- L0 基础设施层 ---
  store/         repo/ SQLite Repository 实现（对应 protocol/repo/ 接口）、search/ BM25、audit/ 事件与决策日志
  observability/ metrics/ Prometheus（TokenBurnRate CANONICAL）、probe/ 硬件与内存探针 + Tier 参数 + FeatureGate、trace/ 链路追踪
  security/      taint/ 五级污点（TaintedString/SafeString）、policy/ Cedar 三层防线 deny-by-default、
                 token/ 能力令牌 Ed25519、network/ SafeDialer + LocalOnly、guard/ Factuality + PII
  llm/           adapter/（anthropic/deepseek/google/ollama/openai）、stt/ Sherpa-ONNX、tts/
  ffi/           Rust dylib 零 CGO 桥接（purego）
  sysinfo/       系统信息采集（硬件探针；供 agent 硬件分级 / sys_probe 工具）
  downloader/    通用资源下载（HTTP/Git + 系统代理探测；模型二进制与插件包共用）
  # sysinfo/ downloader/ 2026-07-07 自 sysmgr/ 迁入 L0：被 L0/L1/L2 广泛引用，不含 L3 治理语义

  # --- 通用契约（所有层均可引用）---
  protocol/      跨模块共享类型 + 接口契约：repo/ 接口定义、pb/ Protobuf 生成物、schema/ DDL SQL（35 个，SSoT）
  config/        配置加载 + 编译期不变量
  lint/          CI 静态扫描规则
  bootstrap/     模块生命周期编排（Bootable + DependencyMap + Kahn 拓扑排序，四阶优雅关停）

pkg/             通用工具（无业务逻辑，任意层可引用）
  apperr/        统一错误类型 apperr.New/Wrap/IsCode/HTTPStatus（禁裸 error 泄漏调用链）
  types/ version/  基础共享类型 / 版本信息

rust/substrate/  Rust FFI 库（Cedar 策略引擎 + SurrealDB-Core，purego 桥接，ADR-0011）
```

- 错误统一 `pkg/apperr`（`apperr.New/Wrap`；禁裸 `errors.New`/`fmt.Errorf` 泄漏调用链）
- `internal/` 禁全局可变变量（并发安全 + 测试隔离；ADR-0001 豁免仅限 observability/metrics 一等公民指标）
- 跨模块走 `internal/protocol/` 结构化事件（禁字符串隐式耦合）
- Rust 仅性能关键 FFI（维持语言边界）
- **[强制] 提交前自检**：在执行 `git commit` 之前，必须先执行 `make lint`（或 `make fmt && make lint`）确保代码风格、圈复杂度等检查全部绿灯。**若本次改动涉及 `docs/arch/` 或包路径迁移，追加 `make docs-refs`**（失效路径引用门控，ADR-0081；白名单 `tools/baselines/docs-refs-allowlist.txt` 仅收"文档在记载已删除/已迁移路径"的历史注记）。
- **[强制] 新增/修改门控规则**：`tools/*_lint.go`、`tools/*_check.go` 必须在 `tools/lint-selftest.txt` 登记负向用例，并使 `make lint-selftest` 通过（注入违规样例→报红→还原→转绿）。**未经负向验证的规则不算 landed**——一个永远不报警的门控与没有门控在 CI 输出上长得一模一样。判定由门控做，不接受自述（2026-08-12 实测：12 条自述 landed 的门控里 6 条抓不到它们声称要防的缺陷）。
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
4. `docs/arch/decisions/ADR-XXXX-*.md` → 决策档案，规则化短文（决策+后果边界+反例守护，不含叙事；同模块多份紧耦合决策合入单一锚点文件，用"决策一/决策二/…"分节）；**"为什么不用 X" 先 grep 这里**——重提已驳方案须带新事实/新约束并在原 ADR 追记复核，不得凭空重议（见 `§文档可修订性`）
5. `docs/arch/spec/state.yaml` → 状态机 + 全模块阈值 SSoT，按 `§par/§staging/§taint/...` 偏移局部读
6. `docs/specs/0X-*.md` → 按域选读：Go↑01 / Rust↑02 / Agent↑03 / 跨模块↑04 / 审查↑06 / 提交前↑06
7. `docs/specs/07-Reference-Implementation.md` → 写新代码前定位 canonical 标瑯
8. `docs/specs/09-LLM-Agent-Production.md` → **写任何 Agent/LLM/Tool/RAG/Memory 相关代码前必读**（A-01~A-14 陷阱 + P-1~P-9 生产原则 + RAG/并发安全检查清单）
9. `internal/protocol/` → 跨模块共享类型与接口契约
10. `internal/protocol/schema/NNN_*.sql` → **DDL Schema SSoT**（001~024 + 028~038，共 35 个 SQL 文件，025~027 保留未用）；修改 Schema 前必读目标表文件，禁 ALTER TABLE 补丁（上线前直接改原始文件 + 删库重建）

**docs/arch/decisions/ 索引**：编号 SSoT = [`decisions/README.md`](docs/arch/decisions/README.md)（权威索引表 + 「已删除」编号对照表 + 编号不重排的理由）。本文件不维护副本（含份数——`make docs-refs` 的 adr-index 项每次运行都会打印实时份数），避免双份索引漂移——按主题词 grep `docs/arch/decisions/` 即可。

**internal/protocol/schema/ DDL 清单**：`ls internal/protocol/schema/*.sql` 即得（35 个）。025~027 编号段**刻意预留**——对应表已重构合并至其他表，编号不复用防历史混淆，不得报为"缺失文件"；`embed.go` 用 `//go:embed *.sql` 自动包含，跳号不影响编译。修改 Schema 前必读目标表文件。

**禁止**：
- 未读 INDEX 直接加载多个 M_X
- 将 `ROADMAP.md` `DIAGRAMS.md` 列为默认加载（人类参考层，按需 §跳读）
- 将 `ARCHITECTURE.md` 全量预读（SSoT 锚点，按场景按 §跳读）
- 以 ALTER TABLE / ADD COLUMN 补丁文件修改 Schema（上线前直接改原始 SQL 文件）

**模块上下文（重要）**：进入 `internal/<X>/` 时，若该目录存在 `internal/<X>/CLAUDE.md` 则必读（当前 action/agent/execute/extension/learning/memory/swarm 及 gateway/server 共 8 个目录有对应文件，其余模块不存在时不适用本条）。
- 各包规范文件名统一为 **CLAUDE.md**（Claude Code 原生自动注入子目录 CLAUDE.md；Gemini / GPT / Cursor 等工具**需手动读取**）
- README.md 为人类导航页，仅重定向至 CLAUDE.md，不含规范内容

**arch ↔ specs 分工**：
- `arch/` = 系统**是什么**（设计）：M_X 实现 / ARCH SSoT 锚点 / 00-Dict 概念 / state.yaml 阈值
- `arch/decisions/` = 决策档案（why-not 单源）：ADR 是"反复被驳的方案"档案，与 M_X 是引用关系
- `specs/` = AI 代码**怎么写**（规范）：R1~R8 反模式 + R2 命名 SSoT + 工作流 + 审查清单

## 文档可修订性

文档分两类，冲突时判定方向**相反**：

| 类 | 范围 | 与代码冲突时 |
|---|---|---|
| **描述性**（记录代码事实） | `arch/ARCHITECTURE.md` 现状章节、`arch/M_X.md` 实现描述、ADR 的事实引用（行数/路径/调用点/接线状态） | **以代码为准**，同步订正文档 |
| **规范性**（约束代码行为） | `§不变量` HE-1~7、`specs/` R1~R8 + R2 命名、`arch/spec/state.yaml` 阈值、`arch/00-Global-Dictionary.md` 概念、`protocol/schema/` DDL | **以文档为准**，改代码 |

判定归属：该段被推翻后，是文档失真，还是代码失控？前者描述性，后者规范性。同段兼具 → 按规范性处置。

规范性条款并非不可推翻，但**必须先改文档再改代码**，且须带门控证据（Eval / lint / 可复现用例）——HE-4 禁止跳过 Eval 直改阈值 / Prompt / 权重。反向操作（先落代码再补文档）等同用既成事实降级不变量，正是 HE-7 防退化边界要拦的。

描述性文档记录写作当时的认知，是记录而非宪法。`§当前阶段` 为代码开发期——判定存在全局更优解时**应当**修订，不得为迁就旧文档而写次优代码。

> 2026-08-07 追记：本节初稿单写"一切以代码为准"，未分两类。实证反例：`internal/llm/circuit_breaker.go` 等多处 Go 注释明写"参数权威源 `spec/state.yaml`"——阈值文档高于代码；按初稿字面，代码绕过 ExecuteTool 写记忆即可反过来"订正" HE-7。故收窄为上表两类模型，原"以代码为准"结论在描述性一类内继续成立。

修订方式：在原节**追记**复核日期 + 推翻理由，原结论保持可见。禁止静默改写使旧决策看起来从未存在——那会让"当初为什么这么定"失去可追溯性，下一个人将重蹈已驳方案。

ADR 补充：`decisions/` 免 `make docs-refs` 扫描（刻意记载历史路径，扫描必然误报），但**免扫 ≠ 免改**（ADR-0081 决策一 2026-08-07 澄清）。ADR 内部同样分两类：**事实引用属描述性**（行数、路径、"是否已接线"），与代码不符直接订正；**决策结论与「重新评估触发条件」属规范性**，重提已驳方案须带新事实/新约束，并在原 ADR 追记复核。

**三项不可变**：ADR 编号不重排/不复用（理由见 `docs/arch/decisions/README.md` 顶部）；上线后已应用的 DDL 迁移文件（见 `§项目结构` DDL 修改策略）；他人 commit 历史。

## 当前阶段

代码开发，覆盖全仓库。规约明确的模块优先开发；规约缺失/模糊 → 编码前补设计。
