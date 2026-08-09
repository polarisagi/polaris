# Architecture Decision Records (ADR)

> 记录非平凡架构决策。AI 修代码前必须 grep 相关 ADR，避免反复提议已被驳回的方案。
> **ADR 正文是规则化短文，非叙事记录**：每份保留"决策 + 后果边界 + 反例守护"，
> 不保留上下文铺垫、被驳回方案逐条论证、修订历史细节——这些留在 git 提交记录里可查，
> ADR 只承载"未来读者需要知道的规则"。
>
> **精简历史**：2026-07-28 三轮压缩，73 份 → 60 份 → 34 份 → 29 份。第一轮合并同主题
> 系列（11 份），第二轮按模块功能 + 跨文档引用关系合并（26 份），第三轮补漏同模块紧耦合
> 决策（5 份：互补技术选型、SSoT+其验证测试、CI 门禁批次、同管线纵深防线、控制流基础决策
> +其后续治理路线）——同一模块的多份"生产接线/新增模式/阶段性加固"系列 ADR、以及正文
> 互相引用（取代/补充/推翻/互补/前置条件）的 ADR 合入单一锚点文件，用"决策一/决策二/…"
> 分节承载，不再各自独立成文件。
>
> **编号不重排**：文件被合并删除后原编号永久留空，不做序号压缩整理。理由：（1）编号本身
> 不承载语义，只是分配顺序标记，稀疏不影响检索——README 索引表与文件名前缀已是唯一入口；
> （2）压缩编号会使已合入内容的"原 ADR-NNNN"历史标注全部失真，且任何指向具体编号的历史
> commit message / 代码注释 / 外部文档都会与新编号错位，属于"篡改历史"而非精简；
> （3）"编号一经分配不复用"是本文件自身写明的既定规则（见下）。

## 何时写 ADR

| 触发场景 | 示例 |
|---------|------|
| 依赖选型 | DB 引擎、库、外部服务 |
| 跨层例外 | 违反 B1 依赖方向的特批 |
| 性能权衡 | 牺牲可读性换性能、放弃通用性换 Tier-0 |
| 安全协议 | 新污点降级路径、新 sandbox 级别 |
| 反复询问 | "为什么不用 X" 已被多次询问 |

不需要 ADR 的：单纯的实现选择、可逆的局部决定、纯重构。

## 编号

按时间递增 4 位数字：`ADR-0001-<kebab-case-title>.md`。编号一经分配不复用——文件被合并删除后，该编号永久留空，不给新决策。

## 状态机

```
Proposed → Accepted ──→ Superseded by ADR-NNNN
                   ──→ Deprecated
```

## 引用纪律

ADR 被代码引用时，源文件头部加：

```go
// ADR: docs/arch/decisions/ADR-0001-sqlite-not-postgres.md
```

若被引用的 ADR 已合并入其他编号，代码注释须同步指向存活编号，不得引用已删除文件。同一编号可能被二次转手合并（如 A 并入 B，B 后又并入 C）——引用须指向最终存活编号，不得停留在中间编号。

## 索引

| 编号 | 标题 | 状态 | 日期 |
|------|------|------|------|
| 0001 | observability 一等公民指标使用包级全局变量（R1.3 豁免） | Accepted | 2026-05-16 |
| 0002 | skill 子包内本地接口/类型消除（R1.4 合规） | Accepted（已执行完毕） | 2026-05-16 |
| 0003 | 主存储引擎选型合集（modernc/sqlite 主持久化 + SurrealDB 认知检索轴，含原 ADR-0010） | Accepted | 2026-05-16 |
| 0004 | Tier-0 8GB 内存硬上限 + Hardware Tier 解锁机制 | Accepted | 2026-05-16 |
| 0006 | state.yaml SSoT + 一致性回归测试合集（含原 ADR-0012） | Accepted | 2026-05-16 |
| 0007 | TaintLevel 五级 + 只升不降 + Sanitizer 受控降级（含原 ADR-0045/0047） | Accepted | 2026-05-16 |
| 0008 | Sandbox 与代码安全防线合集（三级基座 + Logic Collapse L3 + L4 长驻进程池 + 三层代码安全防线，含原 ADR-0024/0026/0078/0079） | Accepted | 2026-05-16 |
| 0009 | KillSwitch 熔断与恢复合集（三阶段熔断 + 进程内活恢复模型，含原 ADR-0072/0073） | Accepted | 2026-05-16 |
| 0011 | purego（零 CGO）FFI 桥接合集（含原 ADR-0005/0030/0034/0063，含 Tree-sitter 例外） | Accepted（已执行） | 2026-05-16 |
| 0013 | CI 质量门禁合集（lint 机械化 Phase 1 + 对抗审查 GitHub Action，含原 ADR-0014） | Accepted（已执行完毕） | 2026-05-16 |
| 0016 | 扩展/插件系统治理合集（统一信任模型 + Codex 特性集成 + 安装/升级生命周期，含原 ADR-0015/0019/0075） | Accepted | 2026-05-21 |
| 0017 | MCP 传输层与协同架构合集（Streamable HTTP + TaintPreservingDecoder + A2A 战略方向，含原 ADR-0018/0070） | Accepted / Proposed | 2026-05-21 |
| 0020 | LLM Provider 选型与推理路由合集（DeepSeek V4 默认 + ThinkingMode 三档，含原 ADR-0022） | Accepted | 2026-06-08 |
| 0021 | 核心机制实现（SurpriseIndex / ScriptTester / BM25 / FSM） | Accepted | 2026-06-09 |
| 0025 | 全局架构审查与系统加固综合档案（含原 ADR-0027/0028/0029/0038） | Accepted | 2026-06-14 |
| 0031 | TTS 三路 Provider 架构（Edge / HTTP / Sherpa） | Accepted | 2026-06-27 |
| 0033 | M05 记忆子系统架构决策合集（写路径 + 范围限制 + 容量压缩，含原 ADR-0023/0035/0036/0060） | Accepted | 2026-06-13 |
| 0042 | Gateway 交互式提案合集：HITL AskUser 咨询闭环 + Generative UI SSE（含原 ADR-0043） | Proposed（均未实现） | 2026-07-11 |
| 0046 | internal/execute 模块创建 + 编排模式演进（模式9 PatternDAG / 模式10 StateGraph / 模式11 Debate-Critic，含原 ADR-0037/0041/0080） | Implemented | 2026-07-13 |
| 0048 | M9 自进化引擎生产接线合集（含原 ADR-0049/0054/0055/0056/0058） | Accepted（已执行） | 2026-07-14 |
| 0062 | 死代码治理：判定方法论 + `make deadcode` 门控（含原 ADR-0050/0051/0052/0053/0061） | Accepted（已执行） | 2026-07-22 |
| 0065 | S_REPLAN 扩展激活重试与降级标记 | Accepted（已执行，回填） | 2026-07-23 |
| 0066 | Gateway 治理合集（控制权移交 FSM + SQL 下沉 + Egress 收紧 + Channel 适配器重构 + ChatOrchestrator 拆分路线，含原 ADR-0039/0064/0067） | Accepted / Proposed | 2026-07-08 |
| 0068 | 开放基准适配器架构（τ-bench/Terminal-Bench） | Accepted（已执行） | 2026-07-23 |
| 0069 | OpenLLMetry 轨迹导出器架构 | Accepted（已执行） | 2026-07-23 |
| 0071 | downloader 出站公网豁免（XR-06） | Accepted（已执行） | 2026-07-23 |
| 0076 | 崩溃恢复回放驱动器 + Task Checkpoint + Outbox 幂等修复合集（含原 ADR-0057/0059） | Accepted（已执行） | 2026-07-22 |
| 0077 | M5↔M10 桥接与实体/关系抽取统一（推翻原 ADR-0074 §3 结论，含原 ADR-0074） | Accepted（已执行） | 2026-07-23 |
| 0081 | 架构文档结构治理（`make docs-refs` 门控 + M07/M13 拆分暂缓，含原 ADR-0044） | Accepted（门控部分已实施）/ Deferred（拆分） | 2026-07-28 |
| 0082 | MemFS：扩展 core_memory_edit 实现显式可编程记忆块 | Accepted（已执行） | 2026-08-02 |
| 0083 | 双时态知识图谱：关系边时态化 + AsOf 视图 | Accepted（已执行） | 2026-08-02 |
| 0084 | MCP A2A：复用 transfer_to_agent 挂起机制补齐出站跨框架委派 | Accepted（已执行） | 2026-08-02 |
| 0085 | 抽取 SessionOrchestrator 领域层收敛会话生命周期编排 | Accepted（已执行） | 2026-08-01 |
| 0086 | Handoff 唤醒事件化 + 崩溃后无损续跑快照 | Accepted（已执行） | 2026-08-01 |
| 0087 | 降级必须显式：cedarLeaks 时间窗 + PII LRU 分区回收 + 沙箱可信来源 opt-in | Accepted（已执行） | 2026-08-01 |
| 0088 | Saga 补偿收敛单一 SSoT + 令牌 claim 兑现 + 工作区上下文信任模型 + HITL 自适应降级 + 跨 Agent Saga 协调 + 检索强化遗忘 | Accepted（已执行） | 2026-08-06 |
| 0089 | lint 规则扫描根接回 internal/ + 裸 error 判定按来源收窄 + 失效路径门控扩展到 .go 注释 | Accepted（已执行） | 2026-08-08 |
| 0090 | 注释里的设计名与代码名：不做符号门控，改为要求实现处锚定 | Accepted（已执行） | 2026-08-09 |
| 0091 | 审计门控的覆盖面而非结果：test-race 改全仓、fuzz 并入 check-all、docs-refs 去 maxdepth | Accepted（已执行） | 2026-08-09 |
| 0092 | docs/ 与提示词的读者定位：AI 优先，reformat（人类可读性）方向视为已驳回 | Accepted（已执行） | 2026-08-09 |
| 0093 | M12 Eval Harness 评测隔离层设计（2026-08-09 由 0051 改号，原编号与已删除 ADR 撞号，见该文件顶部改号说明） | Accepted | 2026-07-28 |

> 代码审查中被驳回的重复性发现（含复现证据），见 `local_playground/upgrade/98-rejected-findings.md`。

## 已删除（内容已合并至目标 ADR，不再保留独立文件）

| 原编号 | 标题 | 合并至（最终存活编号） | 删除日期 |
|------|------|--------|---------|
| 0005 | purego（零 CGO）作为 Go→Rust FFI 桥接方式（原始设计决策） | ADR-0011 | 2026-07-28 |
| 0010 | SurrealDB（Rust FFI 嵌入式）作为认知检索轴 | ADR-0003 | 2026-07-28 |
| 0012 | state.yaml ↔ Go 代码一致性回归测试设计 | ADR-0006 | 2026-07-28 |
| 0014 | 对抗审查 GitHub Action（执行带 3） | ADR-0013 | 2026-07-28 |
| 0015 | Codex 特性集成 | ADR-0016 | 2026-07-28 |
| 0018 | MCP Transport 用 TaintPreservingDecoder | ADR-0017 | 2026-07-28 |
| 0019 | extension_instances 统一安装实例表 | ADR-0016 | 2026-07-28 |
| 0022 | ThinkingMode 三档路由取代 BestOfN/MCTS | ADR-0020 | 2026-07-28 |
| 0023 | episodic 写路径双轨制 | ADR-0033 | 2026-07-28 |
| 0024 | GovernanceAgent 代码安全三层防线 | ADR-0008 | 2026-07-28 |
| 0026 | Logic Collapse 执行运行时：Python + ContainerSandbox | ADR-0008 | 2026-07-28 |
| 0027 | Gemini 执行后遗留实现缺口修复 | ADR-0025 | 2026-07-22 |
| 0028 | Phase 0 P0 Bug 修复 | ADR-0025 | 2026-07-22 |
| 0029 | Phase 1-2 系统加固 | ADR-0025 | 2026-07-28 |
| 0030 | Tier-2 语义嵌入升级（OpenAI 兼容 Embedding + Rust SIMD） | ADR-0011 | 2026-07-28 |
| 0034 | Tree-sitter CGO 例外授权 | ADR-0011 | 2026-07-22 |
| 0035 | 时序记忆检索 + Jaccard 信念修正 | ADR-0033 | 2026-07-22 |
| 0036 | 核心工作记忆区（ZoneCoreMemory） | ADR-0033 | 2026-07-22 |
| 0037 | PatternDAG Orchestration（跨 Agent Macro-DAG 编排模式9） | ADR-0046 | 2026-07-28 |
| 0038 | 影子执行器设计与异步回放选型 | ADR-0025 | 2026-07-22（转手合并 2026-07-28） |
| 0039 | Gateway 控制权移交 FSM（废除 MVP 直通模式） | ADR-0066 | 2026-07-28 |
| 0040 | 受控循环图执行器（CyclicGraphExecutor，未落地草案） | ADR-0046 | 2026-07-22（转手合并 2026-07-28） |
| 0041 | StateGraphExecutor（显式状态图编排，编排模式10） | ADR-0046 | 2026-07-28 |
| 0043 | Generative UI SSE 集成（结构化组件渲染） | ADR-0042 | 2026-07-28 |
| 0044 | M7 模块边界拆分（GD-13-002）暂缓 | ADR-0081 | 2026-07-28 |
| 0045 | 保留五级污点传播（GD-13-004 否决 / GD-14-003 采纳） | ADR-0007 | 2026-07-28 |
| 0047 | taint_sanitizer 二级降级接入 S_VALIDATE | ADR-0007 | 2026-07-28 |

| 0049 | 修复 sCtx.SessionID 从未赋值的根因 Bug | ADR-0048 | 2026-07-28 |
| 0050 | 删除中心化 Orchestrator/Worker/内存 Blackboard 与 SwarmRouter 等 | ADR-0062 | 2026-07-28 |
| 0051 | 跨模块死代码清理与悬空接线收尾（Phase 1-4） | ADR-0062 | 2026-07-28 |
| 0052 | 2026-07-21 全仓库 deadcode 复核收尾 | ADR-0062 | 2026-07-28 |
| 0053 | ADR-0051 遗留 11 项 DEFER 复核 + MCPKnowledgeConnector 接入 | ADR-0062 | 2026-07-28 |
| 0054 | DriftDetector 漂移响应编排器接线 | ADR-0048 | 2026-07-28 |
| 0055 | `/steer` 激活引导命令面接线 | ADR-0048 | 2026-07-28 |
| 0056 | QLoRA/PRM 训练样本采集 + 批次触发流水线 | ADR-0048 | 2026-07-28 |
| 0057 | M04 §8 崩溃恢复回放驱动器 | ADR-0076 | 2026-07-28 |
| 0058 | SICCleaner LLM 检测器接线 | ADR-0048 | 2026-07-28 |
| 0059 | Outbox 幂等键唯一性修复 | ADR-0076 | 2026-07-28 |
| 0060 | M4 ContextWindowManager 热路径压缩接入 | ADR-0033 | 2026-07-28 |
| 0061 | 2026-07-22 deadcode 复核（47 项） | ADR-0062 | 2026-07-28 |
| 0063 | llama_infer 控制面/计算面分离 | ADR-0011 | 2026-07-28 |
| 0064 | Channel 适配器注册表重构 + 统一入站分发接线 | ADR-0066 | 2026-07-28 |
| 0067 | Gateway God Class 拆分（ChatOrchestrator） | ADR-0066 | 2026-07-28 |
| 0070 | MCP Agent-to-Agent (A2A) 协同架构 | ADR-0017 | 2026-07-28 |
| 0072 | KillSwitch 恢复路径统一（内容与 ADR-0073 重复） | ADR-0009 | 2026-07-23（转手合并 2026-07-28） |
| 0073 | KillSwitch 恢复路径统一（进程内活恢复模型） | ADR-0009 | 2026-07-28 |
| 0074 | Semantic(M5) 与 GraphRAG(M10) 最小整合桥接 | ADR-0077 | 2026-07-28 |
| 0075 | Extension Upgrade Versioning | ADR-0016 | 2026-07-28 |
| 0078 | Sandbox-L4-Persistent 接线到位、后端诚实留空 | ADR-0008 | 2026-07-25（转手合并 2026-07-28） |
| 0079 | Sandbox-L4-Persistent 改用长驻解释器进程池 | ADR-0008 | 2026-07-28 |
| 0080 | 新增 Debate/Critic 编排模式（GD-6） | ADR-0046 | 2026-07-28 |

> 现有 `docs/arch/M_X` 文档中的关键决策应回填为 ADR。回填优先级：依赖选型 > 跨层例外 > 性能权衡。

## 模板

见 [`ADR-template.md`](./ADR-template.md)。
