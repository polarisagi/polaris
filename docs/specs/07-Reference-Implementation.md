# 07 参考实现索引

> 每个功能域指定一份 canonical 文件作为标杆。AI 写新代码前必须先 Read 该标杆，结构/命名/错误处理向其对齐。
> 偏离须在 PR 描述中说明原因。

## 7.1 标杆索引

| 模块 | 标杆文件 | 选定理由 | 复制时关注 |
|------|---------|---------|---------|
| internal/store（SQLite KV） | `internal/store/store.go` — SQLiteStore | 纯 Go 通用 storage 范式 | `protocol.Store` 实现、`perrors.Wrap(CodeInternal, msg, err)`、MutationBus 串行写约束、WAL + schema 迁移、`fs.ReadDirFS` 注入 |
| internal/store（FFI 桥接） | `internal/store/surreal_store.go` — SurrealDBCoreStore | FFI 桥接专项标杆（purego→Rust，ABI 1.0） | `purego.RegisterLibFunc` 函数指针绑定、`*byte` + `runtime.KeepAlive` 防 GC 移动、`unsafe.Slice` + `surreal_free_buf` 立即拷贝立即归还、`useHNSW` 模式切换 |
| internal/store（MutationBus 写） | `internal/store/mutation_bus.go` + `mutation_bus_execute.go` | AI 核心数据批量写范式 | `Submit` 投递 intent→channel、`flushBatch` BEGIN→逐条执行→COMMIT、`ResultCh` 同步确认、租约二次校验 |
| internal/observability/metrics | `internal/observability/metrics/metrics.go` | 一等公民指标实现 | `atomic.Int64` + `sync.RWMutex` 并发、双窗 EMA、`ThrottleStage` 三级阶梯、Counter 边沿驱动 KillSwitch。**包级全局变量豁免 ADR-0001**（仅限一等公民指标） |
| internal/security/policy | `internal/security/policy/gate.go` — Cedar Three-Layer Gate | 三层 Cedar 主入口 | deny-by-default、forbid-overrides-permit、fail-closed（>10ms 或异常→deny）、连续 10 次失败触发 KillSwitch Stage 1 |
| internal/llm/adapter | `internal/llm/adapter/anthropic.go` | Provider 适配器范式 | `credentialFn func() string` 凭证 JIT 拉取、`defer clearString(&apiKey)` 内存擦除、`ProviderCapabilities` 完整声明、HTTP 错误规范包装 |
| internal/agent/fsm | `internal/agent/fsm/state_machine.go` — StateMachine | 状态机持有控制流（HE-Rule-5 物理落地） | Transition Guard/Effects 双段拆分、`LLMFillEffect` vs `DeterministicEffect`、`replanCount`+`eventSeq` replay key、S_INTERRUPT 11 态扩展、`UserInterrupt <200ms SLO` |
| internal/memory | `internal/memory/memory.go` — MemImpl | 四层记忆体系 | `Layer` 枚举（Working/Episodic/Semantic/Procedural）、TaintLevel 常量与 protocol 对齐、`ImmutableCore` 守护、`HybridRetrieverImpl` 多路融合 |
| internal/extension/skill（内存注册） | `internal/extension/skill/skill.go` — RegistryImpl + SelectorImpl + WasmSkillExecutor | 内存技能注册 + 启发式选择 + Wasm 执行 stub | 直接存 `protocol.SkillMeta`、"skill:" 前缀强制、HMAC-SHA256 `SignatureValid` 准入、Selector 评分公式（Cap40+Cx30+Pass20+Lat10） |
| internal/extension/skill（持久化后端） | `internal/extension/skill/sqlite_registry.go` — SQLiteRegistryImpl | 持久化技能注册表范式 | SQLite UPSERT、`Capabilities`/`Benchmarks` JSON 列序列化、ON CONFLICT 更新策略 |
| internal/tool | `internal/tool/tool.go` — InMemoryToolRegistry | M7 ToolRegistry 主入口 | ExecuteTool → PolicyGate 五阶段 → Sandbox 分级 → ToolResult；分源 RateLimiter（builtin 100 / MCP 10 / shell 2 QPS）；`policy=nil` 时 deny-by-default |
| internal/swarm/orchestrator | `internal/swarm/orchestrator/blackboard.go` — Blackboard + TaskEntry | M8 多 Agent Blackboard 范式 | `TaskStatus` 单调状态机、`Version atomic.Int32` 防 ABA、CAS 认领、LeaseTTL=60s / Heartbeat=15s±5s / Reaper=1s |
| internal/learning | `internal/learning/engine.go` — Engine | M9 自进化三环架构 | L0~L4 `EvolutionLevel` 阶梯、`FailureClass` 三分（logic/controllable/uncontrollable）、`MEMF` 谬误池接口、`AutoCurriculum` 边缘任务发现、内/中/外三环 |
| internal/knowledge | `internal/knowledge/rag_impl.go` — DefaultIngestionPipeline | M10 RAG 文档摄入范式 | Document → DocTree 分块、`StorageRouter` 路由、`chunk:doc_id:c_id` 键格式、batch_write 模式 |
| internal/eval/harness | `internal/eval/harness/runner.go` — RunnerImpl | M12 Eval 套件执行器 | `protocol.EvalRunner` 实现、suite 二分（training/validation）、`activeRuns CancelFunc` 跟踪、`SQLiteEvalStore` 集成 |
| internal/automation | `internal/automation/scheduler.go` — ResourceGovernor | M13 三级降级资源治理 | 内存/CPU 探针、L1/L2/L3 阈值（1.5GB / 1.0GB / 512MB）、`sync.Cond` 让出准入、并发上限抢占式管理 |
| internal/automation/hitl | `internal/automation/hitl/gateway.go` — GatewayImpl | M13 ESCALATE 协议人工审批网关 | `protocol.HITL` 实现、单点出入、Cedar 策略评估边界 |
| internal/gateway/server（HTTP Handler） | `internal/gateway/server/sysadmin/channels.go` | HTTP Handler 四段式范式 | 输入解析→业务方法→错误 Warn→JSON 编码；SQL 不内嵌 handler；`slog.Warn("server: xxx failed", "err", err)` |
| internal/extension/mcp | `internal/extension/mcp/mcp_manager.go` — MCPManager | MCP 客户端管理范式 | `LoadFromDB` 启动加载、`Add/Remove` 全局注册、`trust_tier≥3` 派生 `Trusted=true`、`sanitizeParentEnv()` stdio 隔离、`TaintPreservingDecoder` 反序列化 |
| internal/extension/marketplace | `internal/extension/marketplace/manager.go` — Manager | 扩展市场安装范式 | `InstallExtension` 下载解压、`removePluginRuntime` 级联硬删、多厂商格式适配（`adapter.ParseManifestDir`） |

> 全部 canonical 已由人工 PR 确认（[canonical] tag），见 `docs/specs/CHANGELOG.md`。
> 部分包（swarm root / memory root / learning root / eval root）不设主 canonical——根层文件多为协调入口，无"复制范式"关系；写新代码时以最近相关子包的 canonical + 进入目录时的 `CLAUDE.md` 的"关键参照文件"区为锚。

## 7.2 使用流程

写新代码前，按序执行：

1. **Read 标杆**：上表对应行的标杆文件，先读后写
2. **Read CLAUDE.md**：进入 `internal/<X>/` 时必读 `internal/<X>/CLAUDE.md`（如存在）
3. **结构对齐**：包内文件顺序、构造函数签名风格、helper 位置
4. **命名对齐**：同类对象使用同一动词/名词词根（见 `docs/arch/00-Global-Dictionary.md §13`）
5. **PR 声明**：`参考实现: internal/xxx/yyy.go | 对齐 | 偏离原因（若有）`

## 7.3 偏离协议

偏离 canonical 风格仅在以下情况允许：

| 情形 | 处置 |
|------|------|
| canonical 本身有缺陷 | 先提交"修复 canonical"PR，再写新代码 |
| 新场景 canonical 无对应 | PR 声明"扩展模式"，60 天内未被引用则回收 |
| 临时实验 | 标记 `// experimental:` 注释 + 关联 ADR |

## 7.4 标杆轮换（季度审查）

新模块 30 天内指定 canonical；旧 canonical 累 5 次"扩展模式"偏离 → 轮换审查；旧件保留并加 `// deprecated as canonical: see <new>`。

> 引用关系：R8 强制 PR 引用本文件；W2 Stage 0 要求 Read 标杆；C8 审查对齐。
