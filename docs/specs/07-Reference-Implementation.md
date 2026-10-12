# 07 参考实现索引

> 每个功能域指定一份 canonical 文件作标杆。写新代码前必须先 Read 该标杆，结构/命名/错误处理向其对齐；偏离须在 PR 描述说明原因。

## 7.1 标杆索引

| 模块 | 标杆文件 | 选定理由 | 复制时关注 |
|------|---------|---------|---------|
| internal/store（SQLite KV） | `internal/store/store.go` — SQLiteStore | 纯 Go 通用 storage 范式 | `protocol.Store` 实现、`apperr.Wrap`、MutationBus 串行写约束、WAL + schema 迁移、`fs.ReadDirFS` 注入 |
| internal/store（FFI 桥接） | `internal/store/surreal_store.go` — SurrealDBCoreStore | FFI 桥接专项标杆（purego→Rust） | `purego.RegisterLibFunc` 绑定、`runtime.KeepAlive` 防 GC 移动、`unsafe.Slice` + `surreal_free_buf` 立即拷贝立即归还 |
| internal/store（MutationBus 写） | `internal/store/mutation_bus.go` + `mutation_bus_execute.go` | AI 核心数据批量写范式 | `Submit` 投递 intent→channel、`flushBatch` BEGIN→逐条执行→COMMIT、`ResultCh` 同步确认、租约二次校验 |
| internal/observability/metrics | `internal/observability/metrics/metrics.go` | 一等公民指标实现 | `atomic.Int64` + `sync.RWMutex`、双窗 EMA、`ThrottleStage` 三级阶梯、Counter 边沿驱动 KillSwitch；包级全局变量豁免见 ADR-0001 |
| internal/security/policy | `internal/security/policy/gate.go` — Cedar Three-Layer Gate | 三层 Cedar 主入口（防退化纵深体系，HE-7；删除或绕过需 ADR 级决策） | deny-by-default、forbid-overrides-permit、超时/异常→deny、连续失败触发 KillSwitch Stage 1（阈值见 state.yaml） |
| internal/security（防退化纵深） | `internal/security/taint/` + `policy/gate.go` + `network/` | 污点五级 + Cedar + KillSwitch + SSRFGuard 标杆（HE-7；删除或弱化需 ADR 级决策） | TaintPropagation、Cedar Three-Layer、KillSwitch、SSRFGuard |
| internal/llm/adapter | `internal/llm/adapter/anthropic.go` | Provider 适配器范式 | `llmparent.CredentialPool` 凭证 JIT 拉取、`ProviderCapabilities` 完整声明、HTTP 错误规范包装 |
| internal/agent/fsm | `internal/agent/fsm/state_machine.go` — StateMachine | 状态机持有控制流（HE-5 物理落地） | Transition Guard/Effects 双段拆分、`protocol.DeterministicEffect` vs LLM 填空 Effect、`replanCount` + `eventSeq` replay key、14 态（含 S_INTERRUPT/S_SUSPENDED/S_AWAIT_AGENT/S_RESPOND） |
| internal/memory | `internal/memory/memory.go` — MemImpl | 四层记忆体系 | `Layer` 枚举、TaintLevel 与 protocol 对齐、`HybridRetriever` 多路融合 |
| internal/extension/skill（内存注册） | `internal/extension/skill/skill.go` — RegistryImpl + SelectorImpl + SkillExecutor | 内存技能注册 + 启发式选择 + Python ContainerSandbox 执行 | 直存 `protocol.SkillMeta`、`"skill:"` 前缀强制、HMAC 签名准入 |
| internal/extension/skill（持久化） | `internal/extension/skill/sqlite_registry.go` — SQLiteRegistryImpl | 持久化技能注册表范式 | `Capabilities`/`Benchmarks` JSON 列序列化、冲突更新策略 |
| internal/tool | `internal/tool/tool.go` — InMemoryToolRegistry | M7 ToolRegistry 主入口 | ExecuteTool → PolicyGate 五阶段 → Sandbox 分级 → ToolResult；分源 RateLimiter；无 policy 时 deny-by-default |
| internal/tool/builtin（记忆写入） | `internal/tool/builtin/memory_tools.go` — `RegisterMemoryTools` | 主动式即时记忆写入标杆（HE-7；删除或绕过需 ADR 级决策） | memory_write/append/expire/page_in/out 等工具定义、`SemanticMemWriter` 消费端接口 |
| internal/execute/orchestrator | `internal/execute/orchestrator/sqlite_blackboard.go` — Blackboard + TaskEntry | M8 多 Agent Blackboard 范式 | `TaskStatus` 单调状态机、CAS 认领、LeaseTTL / Heartbeat / Reaper（阈值见 state.yaml） |
| internal/learning | `internal/learning/engine.go` — Engine | M9 自进化三环架构 | L0~L4 `EvolutionLevel` 阶梯、`FailureClass` 三分、`AutoCurriculum` 边缘任务发现、内/中/外三环 |
| internal/knowledge | `internal/knowledge/rag_impl.go` — DefaultIngestionPipeline | M10 RAG 文档摄入范式 | Document → DocTree 分块、`StorageRouter` 路由、`chunk:doc_id:c_id` 键格式、batch_write |
| internal/eval/harness | `internal/eval/harness/runner.go` — RunnerImpl | M12 Eval 套件执行器 | `protocol.EvalRunner` 实现、suite 二分（training/validation）、`SQLiteEvalStore` 集成 |
| internal/automation | `internal/automation/resource_governor.go` — ResourceGovernor | M13 三级降级资源治理 | 内存/CPU 探针、L1/L2/L3 阈值（见配置）、`sync.Cond` 让出准入 |
| internal/automation/hitl | `internal/automation/hitl/gateway.go` — GatewayImpl | M13 ESCALATE 人工审批网关 | `protocol.HITL` 实现、单点出入、Cedar 策略评估边界 |
| internal/gateway/server（HTTP Handler） | `internal/gateway/server/sysadmin/channelsadmin/channels_crud.go` | HTTP Handler 四段式范式（`01 F9`） | 输入解析 → 业务方法 → 错误 Warn → JSON 编码；SQL 不内嵌 handler |
| internal/extension/mcp | `internal/extension/mcp/mcp_manager.go` — MCPManager | MCP 客户端管理范式 | `StartFromDB` 启动加载（行是配置权威源）、`sanitizeParentEnv()` stdio 隔离、`TaintPreservingDecoder` 反序列化 |
| internal/gateway/authcontext | `internal/gateway/authcontext/contextref.go` — ContextRefExpander | 请求预处理 + 鉴权上下文范式 | `@file`/`@url`/`git:` 引用展开顺序（基础校验后、SlashCommandRouter 前）、`AuthContext` context.Value 注入/提取、匿名兜底 fail-safe |
| internal/extension/marketplace | `internal/extension/marketplace/manager.go` — Manager | 扩展市场安装范式 | `InstallExtension` 唯一安装网关、`CompleteInstall` → `InstallFSM` 分发 |

部分包（swarm / memory / learning / eval 根层）不设主 canonical：根层文件多为协调入口，无"复制范式"关系；写新代码时以最近相关子包的 canonical + 进入目录时 `CLAUDE.md` 的参照文件区为锚。

## 7.2 使用流程

1. **Read 标杆**：上表对应行的标杆文件，先读后写
2. **Read CLAUDE.md**：进入 `internal/<X>/` 时必读（如存在）
3. **结构对齐**：包内文件顺序、构造函数签名风格、helper 位置
4. **命名对齐**：同类对象同一动词/名词词根（`docs/arch/00-Global-Dictionary.md §13`）
5. **PR 声明**：`参考实现: internal/xxx/yyy.go | 对齐 | 偏离原因（若有）`

## 7.3 偏离协议

| 情形 | 处置 |
|------|------|
| canonical 本身有缺陷 | 先提交"修复 canonical"PR，再写新代码 |
| 新场景 canonical 无对应 | PR 声明"扩展模式"，60 天内未被引用则回收 |
| 临时实验 | 标记 `// experimental:` 注释 + 关联 ADR |

## 7.4 标杆轮换

新模块 30 天内指定 canonical；旧 canonical 累计 5 次"扩展模式"偏离 → 轮换审查（季度）；旧件保留并加 `// deprecated as canonical: see <new>`。

引用关系：R8 强制 PR 引用本文件；W2 阶段 0 要求 Read 标杆；C8 审查对齐。
