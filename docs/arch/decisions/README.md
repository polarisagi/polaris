# Architecture Decision Records (ADR)

> 记录非平凡架构决策。AI 修代码前必须 grep 相关 ADR，避免反复提议已被驳回的方案。

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

按时间递增 4 位数字：`ADR-0001-<kebab-case-title>.md`

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

## 索引

| 编号 | 标题 | 状态 | 日期 |
|------|------|------|------|
| 0001 | observability 一等公民指标使用包级全局变量（R1.3 豁免） | Accepted | 2026-05-16 |
| 0002 | skill 子包内本地接口/类型消除（R1.4 合规） | Accepted（已执行完毕） | 2026-05-16 |
| 0003 | modernc/sqlite（零 CGO）作为主持久化存储 | Accepted（回填） | 2026-05-16 |
| 0004 | Tier-0 8GB 内存硬上限 + Hardware Tier 解锁机制 | Accepted（回填） | 2026-05-16 |
| 0005 | purego（零 CGO）作为 Go→Rust FFI 桥接方式 | Accepted（回填） | 2026-05-16 |
| 0006 | state.yaml 作为状态机 + 全模块阈值的 SSoT | Accepted（回填） | 2026-05-16 |
| 0007 | TaintLevel 五级 + 只升不降 + Sanitizer 受控降级 | Accepted（回填） | 2026-05-16 |
| 0008 | Sandbox 三级 + Tier-0 平台特化降级 | Accepted（回填） | 2026-05-16 |
| 0009 | KillSwitch 三阶段熔断 + `.fullstop` 持久状态 | Accepted（回填） | 2026-05-16 |
| 0010 | SurrealDB-Core（Rust FFI）作为认知检索轴 | Accepted（回填） | 2026-05-16 |
| 0011 | cgo → purego 迁移（cedar_ffi.go + surreal_store.go） | Accepted（已执行） | 2026-05-16 |
| 0012 | state.yaml ↔ Go 代码一致性回归测试设计 | Accepted（已执行） | 2026-05-16 |
| 0013 | lint 机械化 Phase 1（depguard / errorlint / nestif / gocyclo） | Accepted（已执行） | 2026-05-16 |
| 0014 | 对抗审查 GitHub Action（执行带 3） | Accepted（已执行） | 2026-05-16 |
| 0015 | Codex 特性集成（Plugin / Hooks / SKILL.md / Custom Agent / CSV fan-out） | Accepted | 2026-05-21 |
| 0016 | 统一信任扩展模型（TrustTier 五级 + 官方 Publisher 白名单） | Accepted | 2026-05-21 |
| 0017 | MCP 默认传输层选 Streamable HTTP，SSE 降级 legacy | Accepted | 2026-05-21 |
| 0018 | MCP Transport 用 TaintPreservingDecoder，禁 encoding/json 直解 | Accepted | 2026-05-21 |
| 0019 | extension_instances 统一安装实例表（三层统一 State-in-DB） | Accepted | 2026-05-22 |
| 0020 | 确立 DeepSeek V4 为全系统默认核心模型（Canonical Provider） | Accepted | 2026-06-08 |
| 0021 | 核心机制实现（SurpriseIndex / ScriptTester / BM25 / FSM） | Accepted | 2026-06-09 |
| 0022 | ThinkingMode 三档路由取代 BestOfN/MCTS 多候选方案 | Accepted | 2026-06-13 |
| 0023 | episodic 写路径双轨制（kv_store 热路径 + OutboxWorker 冷投影） | Accepted | 2026-06-13 |
| 0024 | GovernanceAgent 代码安全三层防线（AST + 正则 + 单次 ThinkingMax LLM） | Accepted | 2026-06-13 |
| 0025 | 全局架构审查缺陷修复综合档案（R21 + 原 ADR-0027 Gemini 缺口 + 原 ADR-0028 Phase 0 缺口） | Accepted | 2026-06-14 |
| 0026 | Logic Collapse Python Skill + ContainerSandbox 运行时 | Accepted | 2026-06-16 |
| 0029 | Phase 1-2 系统加固（AgentPool / VFS 墓碑 / SQL Fitness / SafeGo 全量 / OS Fault 注入 / ShadowExecutor §K，含原 ADR-0038）| Accepted | 2026-06-25 |
| 0030 | Tier-2 语义嵌入升级（OpenAI 兼容 Embedding + Rust SIMD 余弦相似度）| Accepted | 2026-06-20 |
| 0031 | TTS 三路 Provider 架构（Edge / HTTP / Sherpa），默认 Edge TTS 中文高质量 | Accepted | 2026-06-27 |
| 0032 | — | 未分配/未使用 | — |
| 0033 | 记忆子系统范围限制与扩展综合决策（Won't Do + ZoneCoreMemory §决策二/含原 ADR-0036 + 时序检索§决策三/含原 ADR-0035）| Accepted | 2026-07-02 |
| 0037 | PatternDAG Orchestration（跨 Agent Macro-DAG 编排模式9） | Implemented | 2026-07-09 |
| 0039 | Gateway 控制权移交 FSM（废除 MVP 直通模式）| Accepted | 2026-07-08 |
| 0041 | StateGraphExecutor（显式状态图编排，GD-8-001，编排模式10，取代未落地的原 ADR-0040 草案） | Accepted | 2026-07-11 |
| 0042 | HITL AskUser 咨询闭环（AskHuman 特权工具） | Proposed（未实现） | 2026-07-11 |
| 0043 | Generative UI SSE 集成（结构化组件渲染） | Proposed（未实现） | 2026-07-11 |
| 0044 | M7 模块边界拆分（GD-13-002）暂缓 | Deferred | 2026-07-11 |
| 0045 | 保留五级污点传播（GD-13-004 否决 / GD-14-003 采纳） | Accepted | 2026-07-11 |
| 0046 | 新建 internal/execute 模块，收敛单/多 Agent 执行引擎 | Implemented | 2026-07-13 |
| 0047 | taint_sanitizer 二级降级接入 S_VALIDATE，复用 ExemptionVault 而非新建存储 | Accepted（已执行） | 2026-07-14 |
| 0048 | ContinuousSamplingMonitor 写侧接入生产流量，1% 抽样 + LLM Judge 打分 | Accepted（已执行） | 2026-07-14 |
| 0049 | 修复 sCtx.SessionID 从未赋值的根因 Bug（founding_anchor 生产接线前置条件） | Accepted（已执行） | 2026-07-14 |
| 0050 | 删除中心化 Orchestrator/Worker/内存 Blackboard 与 SwarmRouter/CapabilityRegistry/TopologyEvolverService | Accepted（已执行） | 2026-07-14 |
| 0051 | 跨模块死代码清理与悬空接线收尾（Phase 1-4） | Accepted（已执行） | 2026-07-14 |
| 0052 | 2026-07-21 全仓库 deadcode 复核收尾 | Accepted（已执行） | 2026-07-21 |
| 0053 | ADR-0051 遗留 11 项 DEFER 复核 + MCPKnowledgeConnector 接入 | Accepted（已执行） | 2026-07-21 |
| 0054 | DriftDetector 漂移响应编排器接线 + EmbeddingVersionTracker 范围订正 | Accepted（已执行） | 2026-07-21 |
| 0055 | `/steer` 激活引导命令面接线 | Accepted（已执行） | 2026-07-21 |
| 0056 | QLoRA/PRM 训练样本采集 + 批次触发流水线 | Accepted（已执行） | 2026-07-21 |
| 0057 | M04 §8 崩溃恢复回放驱动器 | Accepted（已执行） | 2026-07-22 |
| 0058 | SICCleaner LLM 检测器接线 | Accepted（已执行） | 2026-07-22 |
| 0059 | Outbox 幂等键唯一性修复（非 BuildIdempotencyKey 统一迁移） | Accepted（已执行） | 2026-07-22 |
| 0060 | M4 ContextWindowManager 热路径压缩接入 + M4/M5 共享压缩算法抽取 | Accepted（已执行） | 2026-07-22 |
| 0061 | 2026-07-22 deadcode 复核（47 项，1 项新发现 GoldmarkChunker 已删除，2 项待产品决策） | Accepted（部分已执行） | 2026-07-22 |
| 0062 | deadcode 44 项 DEFER 最终结清（删除为主 + 门控白名单；C2 AddToGate 复核确认删除正确，taint_sanitizer 复核恢复；Tier1 本地默认选型 Qwen3-0.6B 对） | Accepted（已执行） | 2026-07-22 |
| 0063 | llama_infer 控制面/计算面分离（ABORT_FLAG 协作式取消 + status 无锁只读镜像 STATUS；不改单槽位串行推理取舍） | Accepted（已执行） | 2026-07-22 |
| 0064 | Channel 适配器注册表重构（A-1，sync.OnceValue 单例查表）+ 统一入站分发接线（A-2） | Accepted（已执行） | 2026-07-23 |
| 0065 | S_REPLAN 扩展激活重试与降级标记（A-3） | Accepted（已执行，回填） | 2026-07-23 |
| 0066 | Gateway 直连 SQL 下沉 Repository（A-4）+ EgressGateway 收紧默认白名单（A-6） | Accepted（已执行，回填） | 2026-07-23 |
| 0067 | Gateway God Class 拆分（ChatOrchestrator，A-5） | Proposed（设计阶段，未实施） | 2026-07-23 |
| 0068 | 开放基准适配器架构（τ-bench/Terminal-Bench，F-1） | Accepted（已执行） | 2026-07-23 |
| 0069 | OpenLLMetry 轨迹导出器架构（F-2），含 boot/config 接线 | Accepted（已执行） | 2026-07-23 |
| 0070 | MCP Agent-to-Agent (A2A) 协同架构（F-3） | Proposed（战略方向，未落码） | 2026-07-23 |
| 0071 | downloader 出站公网豁免 XR-06（proxy.go） | Accepted（已执行） | 2026-07-23 |

## 已删除（内容已合并至目标 ADR，不再保留独立文件）

| 原编号 | 标题 | 合并至 | 删除日期 |
|------|------|--------|---------|
| 0027 | Gemini 执行后遗留实现缺口修复 | ADR-0025 | 2026-07-22 |
| 0028 | Phase 0 P0 Bug 修复 | ADR-0025 | 2026-07-22 |
| 0034 | Tree-sitter CGO 例外授权 | ADR-0011（§受限例外） | 2026-07-22 |
| 0035 | 时序记忆检索 + Jaccard 信念修正 | ADR-0033（§决策三） | 2026-07-22 |
| 0036 | 核心工作记忆区（ZoneCoreMemory） | ADR-0033（§决策二） | 2026-07-22 |
| 0038 | 影子执行器设计与异步回放选型 | ADR-0029（§K） | 2026-07-22 |
| 0040 | 受控循环图执行器（CyclicGraphExecutor，未落地草案） | ADR-0041（§与 ADR-0040 的关系） | 2026-07-22 |

> 现有 `docs/arch/M_X` 文档中的关键决策应回填为 ADR。回填优先级：依赖选型 > 跨层例外 > 性能权衡。

## 模板

见 [`ADR-template.md`](./ADR-template.md)。
