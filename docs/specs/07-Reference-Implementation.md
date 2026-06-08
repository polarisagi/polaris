# 07 参考实现索引

> 每个 `pkg/` 指定一份 canonical 文件作为标杆。AI 写新代码前必须先 Read 该标杆，结构/命名/错误处理向其对齐。
> 偏离须在 PR 描述中说明原因。

## 7.1 标杆索引

| 模块 | 标杆文件 | 选定理由 | 复制时关注 |
|------|---------|---------|---------|
| pkg/substrate/storage | `pkg/substrate/storage/store.go` — SQLiteStore | 纯 Go 通用 storage 范式 | `protocol.Store` 实现、`perrors.Wrap(CodeInternal, msg, err)`、MutationBus 串行写约束、WAL + schema 迁移、`fs.ReadDirFS` 注入 |
| pkg/substrate/storage（FFI 桥接） | `pkg/substrate/storage/surreal_store.go` — SurrealDBCoreStore | FFI 桥接专项标杆（purego→Rust，ABI 1.0） | `purego.RegisterLibFunc` 函数指针绑定、`*byte` + `runtime.KeepAlive` 防 GC 移动、`unsafe.Slice` + `surreal_free_buf` 立即拷贝立即归还、`useHNSW` 模式切换 |
| pkg/substrate/observability | `pkg/substrate/observability/metrics.go` | 一等公民指标实现 | `atomic.Int64` + `sync.RWMutex` 并发、双窗 EMA、`ThrottleStage` 三级阶梯、Counter 边沿驱动 KillSwitch。**包级全局变量豁免 ADR-0001**（仅限一等公民指标） |
| pkg/substrate/policy | `pkg/substrate/policy/gate.go` — Cedar Three-Layer Gate | 三层 Cedar 主入口 | deny-by-default、forbid-overrides-permit、fail-closed（>10ms 或异常→deny）、连续 10 次失败触发 KillSwitch Stage 1 |
| pkg/substrate/inference | `pkg/substrate/inference/adapter_anthropic.go` | Provider 适配器范式 | `credentialFn func() string` 凭证 JIT 拉取、`defer clearString(&apiKey)` 内存擦除、`ProviderCapabilities` 完整声明、HTTP 错误规范包装 |
| pkg/cognition/kernel | `pkg/cognition/kernel/state_machine.go` — StateMachine | 状态机持有控制流（HE-Rule-5 物理落地） | Transition Guard/Effects 双段拆分、`LLMFillEffect` vs `DeterministicEffect`、`replanCount`+`eventSeq` replay key、S_INTERRUPT 11 态扩展、`UserInterrupt <200ms SLO` |
| pkg/cognition/memory | `pkg/cognition/memory/memory.go` — MemImpl | 四层记忆体系 | `Layer` 枚举（Working/Episodic/Semantic/Procedural）、TaintLevel 常量与 protocol 对齐、`ImmutableCore` 守护、`HybridRetrieverImpl` 多路融合 |
| pkg/cognition/skill（内存注册） | `pkg/cognition/skill/skill.go` — RegistryImpl + SelectorImpl + WasmSkillExecutor | 内存技能注册 + 启发式选择 + Wasm 执行 stub | 直接存 `protocol.SkillMeta`、"skill:" 前缀强制、cosign `SignatureValid` 准入、Selector 评分公式（Cap40+Cx30+Pass20+Lat10） |
| pkg/cognition/skill（持久化后端） | `pkg/cognition/skill/sqlite_registry.go` — SQLiteRegistryImpl | 持久化技能注册表范式 | SQLite UPSERT、`Capabilities`/`Benchmarks` JSON 列序列化、ON CONFLICT 更新策略 |
| pkg/action/tool | `pkg/action/tool/tool.go` — InMemoryToolRegistry | M7 ToolRegistry 主入口 | ExecuteTool → PolicyGate 五阶段 → Sandbox 分级 → ToolResult；分源 RateLimiter（builtin 100 / MCP 10 / shell 2 QPS）；`policy=nil` 时 deny-by-default |
| pkg/swarm（root） | `pkg/swarm/blackboard.go` — Blackboard + TaskEntry | M8 多 Agent Blackboard 范式 | `TaskStatus` 单调状态机、`Version atomic.Int32` 防 ABA、CAS 认领、LeaseTTL=60s / Heartbeat=15s±5s / Reaper=1s |
| pkg/swarm/self_improve | `pkg/swarm/self_improve/engine.go` — Engine | M9 自进化三环架构 | L0~L4 `EvolutionLevel` 阶梯、`FailureClass` 三分（logic/controllable/uncontrollable）、`MEMF` 谬误池接口、`AutoCurriculum` 边缘任务发现、内/中/外三环 |
| pkg/swarm/knowledge | `pkg/swarm/knowledge/rag_impl.go` — DefaultIngestionPipeline | M10 RAG 文档摄入范式 | Document → DocTree 分块、`StorageRouter` 路由、`chunk:doc_id:c_id` 键格式、batch_write 模式 |
| pkg/governance/eval | `pkg/governance/eval/runner.go` — RunnerImpl | M12 Eval 套件执行器 | `protocol.EvalRunner` 实现、suite 二分（training/validation）、`activeRuns CancelFunc` 跟踪、`SQLiteEvalStore` 集成 |
| pkg/edge/scheduler | `pkg/edge/scheduler/scheduler.go` — ResourceGovernor | M13 三级降级资源治理 | 内存/CPU 探针、L1/L2/L3 阈值（1.5GB / 1.0GB / 512MB）、`sync.Cond` 让出准入、并发上限抢占式管理。**⚠ `pkg/edge/scheduler.go` (root) 已 Deprecated，不可作为标杆** |
| pkg/edge/hitl | `pkg/edge/hitl/gateway.go` — GatewayImpl | M13 ESCALATE 协议人工审批网关 | `protocol.HITL` 实现、单点出入、Cedar 策略评估边界 |
| pkg/gateway/server（HTTP Handler） | `pkg/gateway/server/channels.go` | HTTP Handler 四段式范式 | 输入解析→业务方法→错误 Warn→JSON 编码；SQL 不内嵌 handler；`slog.Warn("server: xxx failed", "err", err)` |
| pkg/substrate/inference（LLM 调用） | `pkg/substrate/inference/adapter_anthropic.go` | Provider 适配器 + 出站 HTTP 范式 | `credentialFn()` JIT 拉取、`defer clearString` 内存擦除、`safeHTTPClient`（XR-06）、SSE 帧解析、perrors.Wrap 包装所有 HTTP 错误 |
| pkg/substrate（MutationBus 写） | `pkg/substrate/mutation_bus.go` + `mutation_bus_execute.go` | AI 核心数据批量写范式 | `Submit` 投递 intent→channel、`flushBatch` BEGIN→逐条执行→COMMIT、`ResultCh` 同步确认、租约二次校验 |
| pkg/substrate（Store 同步写） | `pkg/substrate/storage/store.go §Put/Txn` | 中频同步 KV 写范式 | `Store.Put`（同步确认）、`Store.Txn`（CAS/原子操作）；不适合高频批量场景 |

> 全部 canonical 已由人工 PR 确认（[canonical] tag），见 `docs/specs/CHANGELOG.md`。
> 部分包（substrate root / cognition root / swarm patterns / swarm supervisor / governance root）不设主 canonical——根层文件多为独立模块，无"复制范式"关系；写新代码时以最近相关子包的 canonical + 各 `pkg/*/AGENTS.md` 的"关键参照文件"区为锚。

## 7.2 使用流程

写新代码前，按序执行：

1. **Read 标杆**：上表对应行的标杆文件，先读后写
2. **结构对齐**：包内文件顺序、构造函数签名风格、helper 位置
3. **命名对齐**：同类对象使用同一动词/名词词根（见 `docs/arch/00-Global-Dictionary.md §13`）
4. **PR 声明**：`参考实现: pkg/xxx/yyy.go | 对齐 | 偏离原因（若有）`

## 7.3 偏离协议

偏离 canonical 风格仅在以下情况允许：

| 情形 | 处置 |
|------|------|
| canonical 本身有缺陷 | 先提交"修复 canonical"PR，再写新代码 |
| 新场景 canonical 无对应 | PR 声明"扩展模式"，60 天内未被引用则回收 |
| 临时实验 | 标记 `// experimental:` 注释 + 关联 ADR |

## 7.4 标杆轮换（季度审查）

新 `pkg/` 30 天内指定 canonical；旧 canonical 累 5 次"扩展模式"偏离 → 轮换审查；旧件保留并加 `// deprecated as canonical: see <new>`。

> 引用关系：R8 强制 PR 引用本文件；W2 Stage 0 要求 Read 标杆；C8 审查对齐。
