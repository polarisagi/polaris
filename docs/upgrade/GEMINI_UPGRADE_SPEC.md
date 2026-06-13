# Polaris 2026 系统升级开发规范
> 版本：v1.0 | 执行者：Gemini | 审查者：Claude
> 代码已全量备份，可放心修改。

---

## 一、项目背景与强制约束

### 1.1 项目简介
Polaris 是开源自托管 AI Agent 系统。技术栈：Go 1.26+ + Rust 1.94+。

### 1.2 硬约束（违反则拒绝合并）

1. **Tier-0 底线**：全部功能在 8GB 内存下可正常运行。新增内存消耗必须有量化说明。
2. **禁止 `go test ./...`**：测试只能用 `make test`。
3. **提交前必须执行** `make fmt && make lint`，全绿才能提交。
4. **禁裸 error 泄漏**：所有错误统一用 `internal/errors` 包装。
5. **禁全局可变变量**：`pkg/` 目录下不允许包级别的可变全局变量，并发安全用 `sync.Mutex` 或 atomic。
6. **禁止 `import pkg/{governance,edge,gateway}`** 进入 `pkg/cognition` 或 `pkg/swarm`（单向依赖）。
7. **DDL 修改策略**：当前处于上线前阶段。新增表直接新建 `internal/protocol/schema/NNN_*.sql`。修改已有表直接改原文件 + 删库重建（`rm ~/.polarisagi/polaris/data/polaris.db`）。禁止写 ALTER TABLE 补丁。
8. **Git 署名**：所有提交使用 `MrLaoLiAI <polarisagi.online@gmail.com>`。
9. **代码注释中文**，标识符英文（Go/Rust 社区惯例）。
10. **配置变更**：修改 `internal/config/` 结构体后必须执行 `make gen-threshold-examples`。

### 1.3 目录结构（不可改动目录层级）
```
pkg/cognition/    L1: kernel/memory/skill
pkg/action/       L1: 原生内置工具集
pkg/extensions/   L2: 扩展生态（MCP/Plugin/Skill）
pkg/swarm/        L2: orchestrator/self_improve/knowledge
pkg/governance/   L3: eval
pkg/edge/         L3: scheduler
pkg/gateway/      L3: HTTP API

internal/config/          配置
internal/errors/          统一错误类型
internal/protocol/        跨模块共享类型 + DDL
rust/substrate/           Rust FFI 性能路径
```

---

## 二、绝对禁止触碰的代码（已完整实现，不需要改动）

以下文件/模块已经完整实现，**不得修改逻辑，不得重写**：

| 文件 | 原因 |
|---|---|
| `pkg/cognition/fsm.go` | FallbackFSM 断路器完整实现 |
| `pkg/substrate/storage/surreal_store.go` | SurrealDB purego FFI 完整实现 |
| `pkg/swarm/topology/swarm.go` | 多 Agent 拓扑路由完整实现 |
| `pkg/swarm/orchestrator.go` | 任务调度完整实现 |
| `pkg/swarm/blackboard.go` | Blackboard CAS 完整实现 |
| `pkg/substrate/taint.go` | 污点系统完整实现 |
| `pkg/action/capability_token.go` | 能力令牌完整实现 |
| `internal/protocol/schema/001_events.sql` 至 `029_workflows.sql` | DDL 完整，只允许新增文件 |
| `pkg/extensions/native/extension_manager.go` | 扩展安装入口完整实现 |
| `pkg/cognition/compressor.go` 核心压缩逻辑 | Stage1/2/3 完整，只允许扩展接口 |

---

## 三、升级任务总览

| 优先级 | 任务 | 涉及文件 |
|---|---|---|
| P0 | 概念对齐：注释/文档重命名，不改逻辑 | 多处注释 |
| P0 | Agent 角色架构：新增 Memory Agent / Governance Agent 骨架 | 新建文件 |
| P1 | 记忆金字塔：L1 滑动窗口 + L2 蒸馏管道 + OOM Guard | 多处 |
| P1 | 内存压力监控接入 SessionCompressor | `pkg/edge/` + `compressor.go` |
| P1 | AgentHER：成功轨迹回写 SurrealDB | `reflexion.go` |
| P1 | Judge Agent：PRM 打分增加明确裁判角色 | `prm/prm.go` |
| P2 | Wasmtime Warm-Pool | Rust + Go |
| P2 | execute_wasm 工具暴露给 LLM | `code_act.go` + tool loader |
| P2 | spawn_planner 工具 + 主线程挂起/唤醒 | kernel + whisper |
| P2 | MCTS 并行探索池（Planner Pool） | 新建 `pkg/swarm/planner/` |
| P2 | 语义压缩 OutboxWorker handler | 新建 handler |
| P2 | Extension Librarian OutboxWorker handler | 新建 handler |
| P3 | WASI Dry-Run MockProxy | 新建 `pkg/action/mock_proxy.go` |
| P3 | 动态内存配额（Wasm maxPages） | `wasmtime_sandbox.go` |

---

## 四、详细任务说明

### 任务 A：概念对齐（注释/文档，不改功能逻辑）

以下是设计概念与代码的对应关系，需要在注释、文档字符串中对齐，**不改任何执行逻辑**：

#### A1：`pkg/cognition/compressor.go`
- 在文件头注释中增加：
  ```
  // 对应架构概念：L0 极速工作记忆 (Working Memory) 的符号化卸载层 (Symbolic Offloading Layer)。
  // 存根格式 "[offloaded: N bytes → read_tool_ref("xxx")]" 即 <log_ref> 符号指针。
  // Stage 3 Mermaid 注入对应 TencentDB-Agent PruneMem 机制。
  ```
- `PruneToolOutputs` 函数头注释增加：`// 实现 Symbolic Offloading：将超阈值工具输出替换为符号指针，保护 L0 工作记忆。`

#### A2：`pkg/swarm/reflexion.go`
- 文件头注释增加：
  ```
  // ReflexionEngine 是 AgentHER (Hindsight Experience Replay) 的核心引擎。
  // 失败路径：Reflexion 三步（原有实现）。
  // 成功路径：success-after-replan → 完整轨迹提炼 → SurrealDB（见 ReplaySuccess 方法，待实现）。
  ```

#### A3：`pkg/cognition/kernel/agent_execute.go`
- System I FastPath 段注释增加：
  ```
  // System I 快思考路径：SurpriseIndex < 0.3 时直接旁路 LLM，对应三轨推理引擎的"降维"轨道。
  ```
- PRM 多候选段注释增加：
  ```
  // System II 慢思考路径（中级）：PRM Judge Agent 对 N 个 DAG 候选方案打分，
  // 选出最优方案执行。对应三轨推理引擎的"升维-PRM轨道"。
  ```

#### A4：`pkg/swarm/self_improve/engine.go`
- 文件头注释：`// EvolutionLevel L0~L4 对应自我演化的五个层级，L4 需多签名审批。`

#### A5：`pkg/swarm/knowledge/ingester.go` 和 `sync_scheduler.go`
- 文件头注释增加：
  ```
  // 本模块是 Memory Agent（后台常驻记忆管家）的核心能力组件。
  // Memory Agent 通过调用 PipelineImpl.Ingest 和 SyncScheduler 实现 L1→L2 蒸馏。
  ```

---

### 任务 B：新增 Agent 角色架构

新建目录 `pkg/swarm/agents/`，包含以下三个文件。

**重要原则**：这三个 Agent 都是后台常驻 goroutine，**不通过 Orchestrator 的 tasks 表领任务**，不占 Orchestrator maxAgents slot。它们有独立的事件循环，通过 channel 与主 Agent 通信。

#### B1：新建 `pkg/swarm/agents/memory_agent.go`

```go
package agents

// MemoryAgent 后台常驻记忆管家。
//
// 职责：
//   1. 定时将 L1 冷数据（episodic_memory 超过滑动窗口的记录）蒸馏为事实三元组，写入 L2 SurrealDB。
//   2. 监听新工具结果事件，触发 Extension Librarian 索引（通过 outbox 写入）。
//   3. 发现对当前任务有价值的历史经验时，向 WhisperChan 推送耳语线索。
//
// 生命周期：由顶层 cmd/polaris 启动，随进程生命周期常驻。
// 内存约束：蒸馏触发间隔最短 60s（防止 DeepSeek API 调用过频）。
// Tier-0：单次蒸馏调用 LLM 最多处理 20 条 L1 记录，控制 token 消耗。
//
// 与 Orchestrator 关系：无关。不领任务，不占 slot。独立 goroutine。
type MemoryAgent struct {
    db           *sql.DB                    // SQLite，读 episodic_memory
    surreal      SurrealWriterInterface     // SurrealDB 写入接口
    llmInfer     LLMInferFunc               // DeepSeek 蒸馏调用
    whisperChan  chan<- MemoryWhisper       // 向主脑推送耳语线索（非阻塞）
    outboxWriter OutboxWriterInterface      // 写 outbox 触发 Extension Librarian
    memPressure  *atomic.Int32              // 内存压力等级，0=正常，1=中等，2=严重
    
    distillInterval time.Duration           // 蒸馏间隔，默认 60s，内存压力高时延长
    coldWindowAge   time.Duration           // L1 记录超过此时间视为冷数据，默认 30min
    coldWindowCount int                     // 或超过此轮次视为冷数据，默认 100
}

// LLMInferFunc LLM 调用函数类型（依赖注入，可 mock）。
type LLMInferFunc func(ctx context.Context, prompt string) (string, error)

// SurrealWriterInterface 最小化 SurrealDB 写入接口（防止循环依赖）。
type SurrealWriterInterface interface {
    FTSIndex(docID, text string) error
    VecUpsert(id string, embedding []float32) error
    GraphRelate(fromID, edgeType, toID string, weight float64) error
}

// OutboxWriterInterface 最小化 outbox 写入接口。
type OutboxWriterInterface interface {
    Write(ctx context.Context, entry OutboxEntry) error
}

// OutboxEntry outbox 写入条目。
type OutboxEntry struct {
    TargetEngine  string
    Operation     string
    Scope         string
    Payload       []byte
    IdempotencyKey string
}

// MemoryWhisper 记忆管家向主脑推送的耳语线索。
type MemoryWhisper struct {
    Content   string    // 线索内容（≤200字）
    Source    string    // 来源（"episodic" | "semantic" | "reflection"）
    Salience  float64   // 显著度 0.0~1.0
    CreatedAt time.Time
}

// NewMemoryAgent 构造函数，所有字段通过依赖注入，无全局状态。
func NewMemoryAgent(
    db *sql.DB,
    surreal SurrealWriterInterface,
    llmInfer LLMInferFunc,
    whisperChan chan<- MemoryWhisper,
    outboxWriter OutboxWriterInterface,
    memPressure *atomic.Int32,
) *MemoryAgent

// Run 启动 Memory Agent 事件循环（阻塞，调用方用 goroutine 启动）。
// ctx 取消时优雅退出。
func (ma *MemoryAgent) Run(ctx context.Context)

// distillColdEvents 将 L1 冷数据蒸馏为 L2 事实三元组。
// 每次最多处理 20 条，避免单次 LLM 调用过大。
// 蒸馏 Prompt 要求 LLM 输出 JSON 数组：[{"subject":"...","predicate":"...","object":"..."}]
// 蒸馏结果写入 SurrealDB：FTSIndex(三元组文本) + GraphRelate(subject→object, predicate, weight=1.0)
func (ma *MemoryAgent) distillColdEvents(ctx context.Context)

// pushWhisper 检查最近 L2 语义记忆中是否有与当前主脑任务相关的线索，
// 有则非阻塞推送到 whisperChan（满则丢弃，不阻塞）。
func (ma *MemoryAgent) pushWhisper(ctx context.Context, currentGoal string)

// scheduleExtensionLibrarian 当检测到新扩展安装事件时，
// 向 outbox 写入 scope='extension:librarian' 的条目。
func (ma *MemoryAgent) scheduleExtensionLibrarian(ctx context.Context, extID, readmePath string)
```

#### B2：新建 `pkg/swarm/agents/governance_agent.go`

```go
package agents

// GovernanceAgent 后台常驻治理守门人。
//
// 职责：
//   1. 包装现有 PolicyGate（Cedar），作为策略评估的统一入口。
//   2. 管理幂等执行网关：CodeAct 产生副作用前检查 outbox 幂等键，
//      命中则返回历史快照，不产生新的物理副作用。
//   3. 内存压力监控：持续读取系统内存状态，更新共享 MemPressureLevel atomic。
//
// 生命周期：常驻 goroutine，不通过 Orchestrator。
// 与 PolicyGate 关系：GovernanceAgent 内部持有 PolicyGate，对外提供更高级的治理接口。
type GovernanceAgent struct {
    policyGate   protocol.PolicyGate  // 现有 Cedar PolicyGate，保持不变
    db           *sql.DB              // 读写 outbox 幂等键
    memPressure  *atomic.Int32        // 共享内存压力等级（Memory Agent 也读这个）
    probeInterval time.Duration       // 内存探测间隔，默认 5s
}

// MemPressureLevel 内存压力等级。
type MemPressureLevel int32

const (
    MemPressureNormal   MemPressureLevel = 0  // 空闲内存 > 30%
    MemPressureModerate MemPressureLevel = 1  // 空闲内存 10%-30%
    MemPressureCritical MemPressureLevel = 2  // 空闲内存 < 10%
)

// NewGovernanceAgent 构造函数。
func NewGovernanceAgent(policyGate protocol.PolicyGate, db *sql.DB) (*GovernanceAgent, *atomic.Int32)
// 返回 GovernanceAgent 和共享的 memPressure atomic（供 MemoryAgent 和 SessionCompressor 使用）

// Run 启动内存监控循环（阻塞，调用方用 goroutine 启动）。
func (ga *GovernanceAgent) Run(ctx context.Context)

// CheckIdempotent 幂等检查：给定 CodeAct 要执行的操作哈希，
// 查 outbox 表，命中返回 (mockResponse, true)，未命中返回 (nil, false)。
// 哈希算法：SHA256(method + url + body)，截取前 32 字节作为 idempotency_key。
func (ga *GovernanceAgent) CheckIdempotent(ctx context.Context, operationHash string) ([]byte, bool)

// RecordExecution 记录执行成功的操作到 outbox（用于下次幂等命中）。
func (ga *GovernanceAgent) RecordExecution(ctx context.Context, operationHash string, response []byte) error

// probeMemory 探测系统空闲内存，更新 memPressure atomic。
// Linux：读取 /proc/meminfo 的 MemAvailable。
// macOS：使用 vm_stat 命令或 syscall。
// 内存压力阈值：
//   MemPressureNormal   = MemAvailable > TotalMem * 0.30
//   MemPressureModerate = MemAvailable 10%~30%
//   MemPressureCritical = MemAvailable < 10%
func (ga *GovernanceAgent) probeMemory()
```

#### B3：新建 `pkg/swarm/agents/doc.go`

```go
// Package agents 提供 Polaris 多 Agent 系统中的常驻后台 Agent 角色实现。
//
// 角色定义：
//   - MemoryAgent (记忆管家)：L1→L2 蒸馏 + 耳语推送 + Extension Librarian 调度。
//   - GovernanceAgent (治理守门人)：PolicyGate 包装 + 幂等网关 + 内存压力监控。
//
// 与 Orchestrator 的关系：
//   本包中的 Agent 均为常驻 goroutine，通过 channel 与主脑通信，
//   不经过 Orchestrator 的 tasks 表，不消耗 Orchestrator maxAgents slot。
//
// Tier-0 约束：
//   两个 Agent 静止时内存开销 < 5MB（goroutine stack + 结构体）。
//   LLM 调用频率由 distillInterval 和 probeInterval 控制。
package agents
```

---

### 任务 C：记忆金字塔完善

#### C1：SessionCompressor 接入内存压力

修改 `pkg/cognition/compressor.go`：

1. 在 `SessionCompressor` 结构体中增加字段：
   ```go
   memPressure *atomic.Int32  // 由 GovernanceAgent 注入，nil 时忽略
   ```

2. 新增方法：
   ```go
   // InjectMemPressure 注入内存压力指针（由顶层 wire 调用）。
   func (sc *SessionCompressor) InjectMemPressure(p *atomic.Int32)
   ```

3. 修改 `ShouldTrigger` 方法，在现有 token 阈值判断之前增加：
   ```go
   // 内存压力高时降低触发阈值，提前启动压缩
   if sc.memPressure != nil {
       switch MemPressureLevel(sc.memPressure.Load()) {
       case MemPressureModerate:
           thr = thr * 0.65  // 65% → 42%
       case MemPressureCritical:
           thr = thr * 0.35  // 65% → 23%，激进压缩
       }
   }
   ```
   **注意**：`MemPressureLevel` 类型定义在 `pkg/swarm/agents/governance_agent.go`，此处只需读 atomic 值比较，不引入 agents 包（防止循环依赖）。直接比较 int32 值即可：`0=正常, 1=中等, 2=严重`。

#### C2：L1 滑动窗口冷热分离

修改 `pkg/cognition/consolidation.go`（或新建 `pkg/cognition/memory_gc.go`）：

新增函数 `MarkColdEpisodicEvents`，在后台定时执行（由 MemoryAgent 的 Run 循环调用）：

```go
// MarkColdEpisodicEvents 将 L1 中超过时间或轮次阈值的 episodic 记录标记为冷数据。
// 标记方式：在 episodic_memory 表的 meta 字段写入 {"cold": true, "cold_at": <unix>}。
// 触发条件（满足任意一个）：
//   - 记录创建时间 > coldWindowAge（默认 30 分钟）前
//   - 同一 session_id 下记录数超过 coldWindowCount（默认 100 条），最旧的超出部分标记为冷数据
// 冷数据不会被立即删除，等待 MemoryAgent.distillColdEvents 蒸馏后才进入 Janitor 清理流程。
func MarkColdEpisodicEvents(ctx context.Context, db *sql.DB, coldWindowAge time.Duration, coldWindowCount int) error
```

#### C3：L2 SurrealDB OOM Guard

在 `GovernanceAgent.probeMemory()` 中，当检测到 `MemPressureCritical`（空闲内存 < 10%）时，额外执行：

```go
// OOM Guard：内存严重不足时，限制 SurrealDB kv-mem 写入速率。
// 实现：向 outbox 写入一条 target_engine='surrealdb', scope='oom_guard', operation='pause'。
// OutboxWorker 消费后暂停 SurrealDB 写入队列 30s。
// 注意：只暂停写入，不影响读取（Agent 仍可从 L2 召回记忆）。
```

新增 `internal/protocol/schema/030_oom_guard_log.sql`：

```sql
-- 030_oom_guard_log: OOM Guard 触发记录
-- 用于审计内存压力事件，不用于业务逻辑
CREATE TABLE IF NOT EXISTS oom_guard_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    triggered_at INTEGER NOT NULL,  -- Unix 毫秒
    free_mem_mb  INTEGER NOT NULL,  -- 触发时的空闲内存 MB
    total_mem_mb INTEGER NOT NULL,
    action       TEXT    NOT NULL   -- 'pause_surreal_write' | 'resume'
);
CREATE INDEX IF NOT EXISTS idx_oom_guard_time ON oom_guard_log(triggered_at DESC);
```

---

### 任务 D：沙箱系统升级

#### D1：动态内存配额

修改 `pkg/action/tool/wasmtime_sandbox.go`：

1. 修改 `WasmtimeSandbox` 结构体，增加字段：
   ```go
   maxPagesFn func() int32  // 动态计算 Wasm 内存页数上限（nil 时回退默认值 16）
   ```

2. 修改 `NewWasmtimeSandbox` 签名：
   ```go
   func NewWasmtimeSandbox(workspaceDir string, maxPagesFn func() int32) *WasmtimeSandbox
   ```
   `maxPagesFn` 为 nil 时使用硬编码默认值 16（兼容旧调用方）。

3. `Run()` 方法中，将 `16` 替换为：
   ```go
   maxPages := int32(16)
   if s.maxPagesFn != nil {
       maxPages = s.maxPagesFn()
   }
   ```

4. 新建 `pkg/action/tool/wasm_quota.go`，提供标准的 `MaxPagesFromSysEnv` 函数：
   ```go
   // MaxPagesFromSysEnv 根据系统物理内存动态计算 Wasm 内存页数上限。
   // 1 page = 64KB。
   // HT0 (≤8GB)  → 16 pages  (~1MB)
   // HT1 (≤16GB) → 64 pages  (~4MB)
   // HT2 (≤32GB) → 256 pages (~16MB)
   // HT3 (>32GB) → 1024 pages (~64MB)
   // 调用开销：读取 /proc/meminfo 或 syscall，结果缓存 30s（避免频繁 syscall）。
   func MaxPagesFromSysEnv() func() int32
   ```

#### D2：execute_wasm 工具暴露

修改 `pkg/action/code_act.go`：

1. 在 `CodeActRequest.Language` 的合法值中增加 `"wasm"`。

2. `validateExecuteRequest` 中增加 wasm 分支：
   ```go
   case "wasm":
       // wasm 路径不需要临时文件，ScriptBytes 直接传给 WasmtimeSandbox
       // 但仍需 CapabilityToken 和 PolicyGate 检查
   ```

3. `Execute` 方法中增加 wasm 语言处理分支：
   ```go
   case "wasm":
       // 不写临时文件，直接构造 SandboxSpec.ScriptBytes = []byte(req.Code)（base64 解码后的字节）
       // 走 WasmtimeSandbox.Run()，而非 NativeSandbox
   ```

4. 修改 `pkg/action/tool/loader.go` 或 `builtin_tools.go`，将 `execute_wasm` 暴露为内置工具，工具描述：
   ```
   name: execute_wasm
   description: 执行 Wasm 字节码（Code-as-Calculus 中维轨道）。
     适用于批处理循环、文件批量操作、数据转换等需要 for 循环的任务。
     比 JSON 工具调用减少 N-1 次 LLM 往返通信。
     input: base64 编码的 Wasm 字节码。
     language: "wasm"
   ```

#### D3：Wasmtime Warm-Pool（Rust 侧）

修改 `rust/substrate/src/` 下的 Wasmtime 相关文件（具体文件名以实际 Rust 目录为准）：

1. 新增导出函数 `wasmtime_pool_init(n: i32) -> i32`：
   - 预初始化 `n` 个 Wasmtime Engine + Store 实例，放入线程安全的对象池（使用 `std::sync::Mutex<Vec<...>>`）。
   - 返回 0 表示成功，非 0 表示失败。
   - 幂等：多次调用只初始化一次（`std::sync::Once`）。

2. 修改 `WasmtimeExecute`（FFI 函数）：执行前先从池借出实例，执行后归还；池空时回退到即时创建（不阻塞）。

3. 在 `pkg/action/tool/wasmtime_sandbox.go` 中，`NewWasmtimeSandbox` 时调用 `WasmtimePoolInit(5)`（FFI）。

#### D4：WASI Dry-Run MockProxy

新建 `pkg/action/mock_proxy.go`：

```go
// MockProxy 是 MCTS 试运行期间的本地 HTTP 代理服务器。
//
// 工作原理：
//   在 loopback 端口（默认 :0，动态分配）启动 HTTP 代理。
//   沙箱通过 HTTP_PROXY 环境变量将所有 HTTP 流量路由到此代理。
//   代理拦截所有非 loopback 目标请求，根据 MockTable 返回预生成响应。
//
// Dry-Run 模式下：
//   WasmtimeSandbox 的 networkAllowed 改为"允许 loopback"（连接 MockProxy），
//   禁止直接访问公网（WASI 层仍然隔离公网）。
//
// MockTable 来源：
//   MCTS session 创建时，由 PlannerPool 调用 DeepSeek 分析代码预期 API 调用，
//   预生成 MockTable（格式：map[string]MockResponse，key = SHA256(method+url+body前1KB)）。
//   未命中时兜底返回 HTTP 200 + `{}`。
type MockProxy struct {
    mockTable   map[string]MockResponse  // 预生成 Mock 响应表
    listener    net.Listener             // 动态端口
    mu          sync.RWMutex
}

// MockResponse 单条 Mock 响应。
type MockResponse struct {
    StatusCode int               // 默认 200
    Body       json.RawMessage   // 响应体 JSON
    Headers    map[string]string // 可选响应头
}

// NewMockProxy 创建并启动 MockProxy，返回代理监听地址（如 "127.0.0.1:54321"）。
func NewMockProxy(mockTable map[string]MockResponse) (*MockProxy, string, error)

// Close 关闭代理。
func (mp *MockProxy) Close() error

// EnvVars 返回需要注入沙箱的环境变量（HTTP_PROXY, HTTPS_PROXY）。
func (mp *MockProxy) EnvVars() map[string]string
```

同时修改 `pkg/action/tool/wasmtime_sandbox.go`，在 `SandboxSpec` 中增加：
```go
DryRunMode   bool              // true = MCTS 试运行，启用 MockProxy
MockProxyEnv map[string]string // MockProxy 环境变量（由 PlannerPool 注入）
```

---

### 任务 E：推理引擎新功能

#### E1：MemoryWhisper 基础设施

新建 `pkg/cognition/kernel/whisper.go`：

```go
package kernel

// MemoryWhisper 来自 MemoryAgent 的耳语线索（异步推送到主脑）。
type MemoryWhisper struct {
    Content   string  // 线索内容 ≤200字
    Source    string  // "episodic" | "semantic" | "reflection"
    Salience  float64 // 0.0~1.0
}

// WhisperChan 耳语通道容量。
// 16 条缓冲：Memory Agent 产出速率远低于 Agent 消费速率，不会积压。
const WhisperChanCap = 16
```

修改 `pkg/cognition/kernel/agent.go`，在 `Agent` 结构体中增加：
```go
whisperChan <-chan MemoryWhisper  // 接收 MemoryAgent 耳语（只读）
```

新增方法：
```go
// InjectWhisperChan 注入耳语接收通道（由顶层 wire 调用，可 nil）。
func (a *Agent) InjectWhisperChan(ch <-chan MemoryWhisper)
```

修改 `pkg/cognition/kernel/memory_context.go` 的 `buildPerceiveContext` 函数，在组装 `baseContent` 时，非阻塞消费 whisperChan：

```go
// 耳语线索注入（非阻塞，Memory Agent 推送的历史经验线索）
if a.whisperChan != nil {
    select {
    case w := <-a.whisperChan:
        if w.Salience >= 0.5 {  // 低显著度线索过滤
            baseContent += fmt.Sprintf("\n## Memory Whisper (source: %s)\n%s\n", w.Source, w.Content)
        }
    default:
        // 无线索，继续
    }
}
```

#### E2：spawn_planner 工具

新建 `pkg/swarm/planner/` 目录，包含以下文件。

**E2a：新建 `pkg/swarm/planner/doc.go`**

```go
// Package planner 实现 Polaris 三轨推理引擎的 System II 慢思考轨道。
//
// 核心机制：
//   主脑调用 spawn_planner(goal, task_type) 工具后，本包接管规划任务。
//   PlannerPool 并发启动 N 个 Worker（Tier-0 默认 3，HT1+ 最多 8），
//   每个 Worker 独立尝试不同策略，最终由 DualEngineScorer 选出最优方案。
//
// 双引擎打分架构 (Dual-Engine Scoring)：
//   - Engine A (CodeAct 任务)：真实 Wasm 沙箱编译 + 单测 reward function。
//   - Engine B (通用 Agent 任务)：Judge Agent LLM 打分（安全性 + 目标对齐度）。
//
// 与 Orchestrator 关系：PlannerPool 不通过 Blackboard，直接启动 goroutine 集群。
// 结果通过 WhisperChan 返回主脑（唤醒挂起的主 Agent）。
package planner
```

**E2b：新建 `pkg/swarm/planner/pool.go`**

```go
package planner

// PlannerPool 并行探索池（System II 慢思考的工作者集群）。
//
// 触发条件：主脑调用 spawn_planner 工具后，Kernel 创建 PlannerPool 并异步启动。
// 主脑通过 Agent.Interrupt(InterruptResume) 挂起自身，等待 WhisperChan 唤醒。
//
// Worker 数量：
//   Tier-0 (≤8GB)  → 3 workers
//   HT1   (≤16GB) → 5 workers
//   HT2+  (>16GB) → 8 workers
//   由 sysenv.GetSystemInfo() 在构造时决定，不可运行时修改。
type PlannerPool struct {
    goal        string
    taskType    string           // "code_act" | "general"
    workers     int              // worker 数量（由 sysenv 决定）
    provider    protocol.Provider
    scorer      DualEngineScorer
    whisperChan chan<- kernel.MemoryWhisper  // 结果返回通道
    mockProxy   *action.MockProxy           // Dry-Run 模式（仅 code_act 任务）
}

// DualEngineScorer 双引擎打分接口。
type DualEngineScorer interface {
    Score(ctx context.Context, goal string, taskType string, candidate *PlanCandidate) (float64, error)
}

// PlanCandidate 单个 Worker 产出的方案。
type PlanCandidate struct {
    DAGModel      *protocol.DAGModel  // 通用 Agent 任务的 DAG 方案
    CodePatch     string              // CodeAct 任务的代码补丁
    CompileResult *CompileResult      // Engine A 评估结果（仅 code_act）
    LLMScore      float64             // Engine B 评估结果（仅 general）
    WorkerID      int
}

// CompileResult Wasm 编译 + 单测结果。
type CompileResult struct {
    ExitCode    int
    TestsPassed int
    TestsFailed int
    ErrorOutput string
}

// NewPlannerPool 构造函数。
// taskType 决定启用哪个 Engine：
//   "code_act" → Engine A（真实编译 + 单测）+ DryRun MockProxy
//   其他       → Engine B（Judge Agent LLM 打分）
func NewPlannerPool(
    goal, taskType string,
    provider protocol.Provider,
    whisperChan chan<- kernel.MemoryWhisper,
) *PlannerPool

// Run 启动并行探索，阻塞直到所有 Worker 完成或 ctx 取消。
// 选出得分最高的 PlanCandidate，格式化为 MemoryWhisper 推送给主脑。
// 消息格式："[PLANNER_RESULT] " + JSON(winning_candidate)
// 主脑在 buildPerceiveContext 消费后，直接使用该方案，不再调用 LLM 规划。
func (pp *PlannerPool) Run(ctx context.Context)

// workerRun 单个 Worker 的执行逻辑。
// code_act 任务：
//   1. 调用 LLM 生成代码补丁（不同 Worker 用不同系统提示词，产生多样性）
//   2. 启动 DryRun MockProxy
//   3. 推入 WasmtimeSandbox 编译
//   4. 运行单测（如有）
//   5. 计算 CompileResult
// general 任务：
//   1. 调用 LLM 生成 DAGModel 方案（不同 temperature，产生多样性）
//   2. 提交 Judge Agent 打分
func (pp *PlannerPool) workerRun(ctx context.Context, workerID int) PlanCandidate
```

**E2c：新建 `pkg/swarm/planner/judge.go`**

```go
package planner

// JudgeAgent Engine B 裁判智能体。
//
// 职责：对通用 Agent 任务（非 code_act）的 DAG 方案进行安全性 + 目标对齐度打分。
// 实现：调用一次 LLM（DeepSeek），使用专属系统提示词扮演"安全审查员"角色。
// 与 DefaultPRM 的区别：
//   DefaultPRM（现有）：用于 Kernel S_PLAN 阶段，基于任务复杂度自动触发，角色是"方案评估器"。
//   JudgeAgent（新增）：用于 PlannerPool，由 spawn_planner 显式触发，角色是"安全 + 对齐裁判"。
//   两者使用不同的 Prompt，服务于不同的触发场景，共存不冲突。
type JudgeAgent struct {
    provider protocol.Provider
    model    string  // 打分模型，默认 "deepseek-chat"
}

// judgeSystemPrompt 裁判系统提示词。
// 与 DefaultPRM 的 scoreCandidate prompt 不同，强调安全性和对齐度。
const judgeSystemPrompt = `你是 Polaris AI 系统的安全与对齐裁判 (Judge Agent)。
你的任务是对给定的执行方案进行评估，输出 0.0~1.0 的评分。
评分标准（按权重）：
1. 安全性 (40%)：方案是否可能产生不可逆副作用、数据泄露、越权操作？
2. 目标对齐度 (40%)：方案是否能真正完成用户描述的目标？
3. 简洁性 (20%)：方案步骤是否冗余？
输出严格 JSON 格式：{"score": 0.85, "safety_issues": [], "reason": "..."}`

// Score 实现 DualEngineScorer 接口（Engine B 路径）。
func (ja *JudgeAgent) Score(ctx context.Context, goal string, taskType string, candidate *PlanCandidate) (float64, error)
```

#### E3：spawn_planner 工具注册

修改 `pkg/action/tool/builtin_tools.go` 或 `loader.go`，将 `spawn_planner` 注册为内置工具：

```
name: spawn_planner
description: 升维工具（System II 慢思考轨道）。
  当任务复杂度高、破坏性风险高，或需要在多个方案中选最优时，调用此工具。
  调用后主脑挂起，后台启动 PlannerPool 并行探索。
  探索完成后，最优方案作为 Memory Whisper 推送回主脑，主脑自动恢复执行。
  参数：
    goal: string       # 任务目标（自然语言）
    task_type: string  # "code_act" | "general"
```

修改 `pkg/cognition/kernel/agent_execute.go`，在工具执行路径中识别 `spawn_planner` 工具调用：

```go
// spawn_planner 特殊处理：不走普通工具执行路径，而是：
// 1. 创建 PlannerPool
// 2. 发送 InterruptRequest{Action: InterruptResume}（挂起自身，等待 whisperChan）
// 3. 启动 goroutine 执行 PlannerPool.Run(ctx)
// 4. PlannerPool 完成后往 whisperChan 推送结果
// 5. 主脑在下一次 buildPerceiveContext 中消费结果
```

#### E4：AgentHER 成功轨迹回写

修改 `pkg/swarm/reflexion.go`：

1. `Reflect` 方法增加参数 `replanCount int`（从 `FallbackFSM.replanCount` 传入）。

2. 原有 `if result.Success { return nil, nil }` 逻辑改为：
   ```go
   if result.Success {
       // AgentHER：如果是经过 replan 后才成功（replanCount > 0），
       // 这是宝贵的"犯错→探索→纠偏"轨迹，写入 SurrealDB 技能库
       if replanCount > 0 && len(trajectory) > 0 {
           return re.replaySuccess(ctx, taskID, taskType, trajectory, replanCount)
       }
       return nil, nil
   }
   ```

3. 新增 `replaySuccess` 方法：
   ```go
   // replaySuccess 将成功纠偏轨迹提炼为 SurrealDB 技能记忆（AgentHER 核心）。
   //
   // 处理流程：
   //   1. 调用 LLM，输入完整 trajectory（含失败步骤和最终成功步骤）
   //   2. Prompt：提炼这次"犯错→成功"的关键洞察，输出 {"insight": "...", "tags": [...]}
   //   3. 将 insight 写入 SurrealDB：
   //      - FTSIndex(docID=taskID+"_her", text=insight)
   //      - GraphRelate(taskType, "learned_from_failure", insight_id, weight=float64(replanCount))
   //   4. 同时写入 reflection_memory 表：reflection_type='success_pattern'
   //
   // 注意：replaySuccess 异步执行（goroutine），不阻塞主反思流程。
   func (re *ReflexionEngine) replaySuccess(
       ctx context.Context,
       taskID, taskType string,
       trajectory []Step,
       replanCount int,
   ) (*Reflection, error)
   ```

#### E5：语义压缩 OutboxWorker Handler

新建 `pkg/swarm/agents/semantic_compress_handler.go`：

```go
package agents

// SemanticCompressHandler 语义压缩 OutboxWorker 处理器。
//
// 触发条件：ToolRefOffloader 将大型工具输出（尤其是 C++/Rust 报错堆栈）卸载到 VFS 后，
//   同时向 outbox 写入 target_engine='semantic_compress', scope='error_stack' 的条目。
//
// 处理流程：
//   1. 从 VFS 读取原始 blob（通过 workspace_vfs 表找到文件路径）
//   2. 调用 DeepSeek，输入原始报错，要求输出结构化 JSON：
//      {"root_cause": "...", "error_type": "...", "suggest_fix": "...", "affected_file": "..."}
//   3. 将结构化 JSON 写回 VFS，替换原始 blob
//   4. 更新 outbox 条目状态为 'done'
//
// 主脑通过 read_tool_ref 读取时，自动拿到精炼后的 JSON，而非原始 5MB 堆栈。
// 这保护了 L0 工作记忆，避免长错误堆栈污染注意力。
type SemanticCompressHandler struct {
    db       *sql.DB
    vfsBase  string              // VFS 文件根目录
    llmInfer LLMInferFunc
}

// Handle 实现 OutboxWorker 的 handler 接口。
func (h *SemanticCompressHandler) Handle(ctx context.Context, entry OutboxEntry) error

// targetEngine 返回此 handler 处理的 target_engine 值。
func (h *SemanticCompressHandler) TargetEngine() string { return "semantic_compress" }
```

同时修改 `pkg/cognition/compressor.go` 的 `prunePartsToolOutputs` 函数：
在 `offloader.Offload(id, rawData)` 调用成功后，额外向 outbox 写入语义压缩任务：
```go
// 若卸载的内容疑似为错误堆栈（启发式检测：包含 "panic", "stack trace", "goroutine", "Error:" 等关键词），
// 写入 outbox 触发语义压缩。
if looksLikeErrorStack(rawData) && outboxWriter != nil {
    _ = outboxWriter.Write(ctx, OutboxEntry{
        TargetEngine:   "semantic_compress",
        Operation:      "compress",
        Scope:          "error_stack",
        Payload:        []byte(`{"vfs_id":"` + id + `"}`),
        IdempotencyKey: "semantic_compress:error_stack:" + id,
    })
}
```

#### E6：Extension Librarian OutboxWorker Handler

新建 `pkg/swarm/agents/extension_librarian_handler.go`：

```go
package agents

// ExtensionLibrarianHandler Extension 司书 OutboxWorker 处理器。
//
// 触发条件：扩展安装成功后（pkg/extensions/native/extension_manager.go InstallExtension），
//   向 outbox 写入 target_engine='extension_librarian', scope='index_extension' 条目。
//
// 处理流程：
//   1. 从 extension_instances 表读取 extID 的元数据（name, publisher, config）
//   2. 查找扩展的 README 文件或 schema 文件（从 install_path）
//   3. 调用 DeepSeek，输入扩展文档，要求输出能力摘要：
//      {"summary": "...", "capabilities": [...], "best_for": [...], "avoid_when": [...]}
//   4. 将摘要写入 SurrealDB：
//      - FTSIndex(docID="ext_"+extID, text=summary+capabilities描述)
//      - VecUpsert（调用 embedding API 向量化 summary）
//      - GraphRelate("extension", "provides_capability", capability, weight=1.0) 对每个 capability
//   5. 更新 outbox 条目状态为 'done'
//
// 主脑在工具不足时，通过 SurrealDB 语义检索快速定位最适合的扩展 ID。
type ExtensionLibrarianHandler struct {
    db          *sql.DB
    surreal     SurrealWriterInterface
    llmInfer    LLMInferFunc
    embedFn     EmbedFunc           // 向量化函数（调用 DeepSeek/OpenAI embedding API）
    vfsBase     string
}

// EmbedFunc 文本向量化函数类型（依赖注入）。
type EmbedFunc func(ctx context.Context, text string) ([]float32, error)

func (h *ExtensionLibrarianHandler) Handle(ctx context.Context, entry OutboxEntry) error
func (h *ExtensionLibrarianHandler) TargetEngine() string { return "extension_librarian" }
```

同时修改 `pkg/extensions/native/extension_manager.go`，在 `InstallExtension` 成功写入 `extension_instances` 表后，追加向 outbox 写入 Extension Librarian 任务的逻辑：

```go
// 安装成功后触发 Extension Librarian 索引（异步，不阻塞安装流程）
// outbox 写入由 MutationBus 处理，幂等键确保重复安装不重复索引
if outboxWriter != nil {
    _ = outboxWriter.Write(ctx, OutboxEntry{
        TargetEngine:   "extension_librarian",
        Operation:      "index",
        Scope:          "index_extension",
        Payload:        []byte(`{"ext_id":"` + instanceID + `"}`),
        IdempotencyKey: "extension_librarian:index:" + instanceID,
    })
}
```

---

### 任务 F：新增 SQL Schema

#### F1：新建 `internal/protocol/schema/031_planner_sessions.sql`

```sql
-- 031_planner_sessions: MCTS 规划会话记录
-- 架构角色: PlannerPool 执行记录，用于审计和经验积累
CREATE TABLE IF NOT EXISTS planner_sessions (
    id              TEXT    PRIMARY KEY,         -- "plan_{UUID}"
    task_id         TEXT    NOT NULL DEFAULT '', -- 关联的 tasks.task_id
    goal            TEXT    NOT NULL,
    task_type       TEXT    NOT NULL,            -- 'code_act' | 'general'
    worker_count    INTEGER NOT NULL DEFAULT 3,
    winning_score   REAL    NOT NULL DEFAULT 0.0,
    winning_engine  TEXT    NOT NULL DEFAULT '', -- 'engine_a' | 'engine_b'
    status          TEXT    NOT NULL DEFAULT 'running', -- 'running' | 'done' | 'failed'
    created_at      INTEGER NOT NULL,
    completed_at    INTEGER
) STRICT;

CREATE INDEX IF NOT EXISTS idx_planner_task ON planner_sessions(task_id);
CREATE INDEX IF NOT EXISTS idx_planner_status ON planner_sessions(status, created_at DESC);
```

#### F2：新建 `internal/protocol/schema/032_mock_response_cache.sql`

```sql
-- 032_mock_response_cache: WASI Dry-Run MockProxy 响应缓存
-- 架构角色: PlannerPool 预生成的 Mock 响应表，避免 MCTS 试运行产生真实副作用
CREATE TABLE IF NOT EXISTS mock_response_cache (
    operation_hash  TEXT    PRIMARY KEY,   -- SHA256(method+url+body前1KB)，前32字节hex
    plan_session_id TEXT    NOT NULL,      -- 关联 planner_sessions.id
    method          TEXT    NOT NULL,      -- HTTP 方法
    url_pattern     TEXT    NOT NULL,      -- URL（可含通配符前缀）
    status_code     INTEGER NOT NULL DEFAULT 200,
    response_body   TEXT    NOT NULL DEFAULT '{}',
    hit_count       INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER               -- 为 NULL 则跟随 planner_session 生命周期
) STRICT;

CREATE INDEX IF NOT EXISTS idx_mock_session ON mock_response_cache(plan_session_id);
```

---

## 五、新增包初始化文件

所有新建包必须有 `doc.go` 文件，包含包职责说明、架构位置、与相邻包的依赖关系。

---

## 六、验收标准

### 6.1 必须通过

```bash
make fmt      # 无输出
make lint     # 无 error/warning
make test     # 全部 PASS
make build    # 二进制构建成功
```

### 6.2 新文件检查清单

- [ ] `pkg/swarm/agents/memory_agent.go` 存在，`Run()` 可被 `go vet` 通过
- [ ] `pkg/swarm/agents/governance_agent.go` 存在，`probeMemory()` 在 Linux 和 macOS 均有实现
- [ ] `pkg/swarm/planner/pool.go` 存在，`PlannerPool.Run()` 实现完整
- [ ] `pkg/swarm/planner/judge.go` 存在，`JudgeAgent.Score()` 实现完整
- [ ] `pkg/action/mock_proxy.go` 存在，`NewMockProxy()` 可用
- [ ] `pkg/action/tool/wasm_quota.go` 存在，`MaxPagesFromSysEnv()` 实现完整
- [ ] `pkg/cognition/kernel/whisper.go` 存在
- [ ] `pkg/swarm/agents/semantic_compress_handler.go` 存在
- [ ] `pkg/swarm/agents/extension_librarian_handler.go` 存在
- [ ] `internal/protocol/schema/031_planner_sessions.sql` 存在
- [ ] `internal/protocol/schema/032_mock_response_cache.sql` 存在

### 6.3 修改文件检查清单

- [ ] `pkg/cognition/compressor.go`：`ShouldTrigger` 有 memPressure 分支
- [ ] `pkg/cognition/kernel/agent.go`：有 `whisperChan` 字段和 `InjectWhisperChan`
- [ ] `pkg/cognition/kernel/memory_context.go`：`buildPerceiveContext` 非阻塞消费 whisperChan
- [ ] `pkg/action/code_act.go`：支持 `"wasm"` 语言
- [ ] `pkg/action/tool/wasmtime_sandbox.go`：`maxPagesFn` 字段，动态配额
- [ ] `pkg/swarm/reflexion.go`：`Reflect` 有 `replanCount` 参数，`replaySuccess` 已实现
- [ ] `pkg/cognition/prm/prm.go`：注释对齐 Judge Agent vs DefaultPRM 的定位区别

### 6.4 架构约束检查

- [ ] `pkg/swarm/agents/` 不引入 `pkg/{governance,edge,gateway}`
- [ ] `pkg/swarm/planner/` 不引入 `pkg/cognition/kernel`（只引入 `protocol` 和 `action`）
- [ ] 所有新增 goroutine 均接受 `ctx context.Context`，可通过 cancel 优雅退出
- [ ] 所有新增 LLM 调用均有 timeout（通过 ctx 传递，默认 30s）
- [ ] 所有新增数据库操作均使用 `internal/errors` 包装错误

### 6.5 Tier-0 内存约束

- [ ] 新增常驻 goroutine 静止时内存开销 < 5MB（可通过 `runtime.MemStats` 验证）
- [ ] PlannerPool Tier-0 下 worker 数不超过 3（`sysenv` 动态决定）
- [ ] MockProxy 动态端口，测试结束后关闭释放

---

## 七、开发顺序建议

按以下顺序开发，每完成一步执行 `make test` 确认不回归：

1. **任务 A**（纯注释，零风险）
2. **任务 C1**（memPressure 接入，小改）
3. **任务 B**（新建 agents 包骨架，可暂时是空实现）
4. **任务 E1**（whisper 基础设施，影响面小）
5. **任务 E4**（AgentHER，改 reflexion.go）
6. **任务 D1 + D2**（动态配额 + execute_wasm）
7. **任务 C2 + C3**（滑动窗口 + OOM Guard）
8. **任务 E5 + E6**（两个 OutboxWorker handler）
9. **任务 B**（补充 Memory Agent + Governance Agent 完整实现）
10. **任务 F**（新增 SQL schema）
11. **任务 E2 + E3**（spawn_planner + PlannerPool，最复杂）
12. **任务 D3**（Wasmtime Warm-Pool，Rust 侧）
13. **任务 D4**（MockProxy，最后）

---

## 八、禁止事项

- **禁止**删除或重写 `pkg/cognition/fsm.go` 的任何现有逻辑
- **禁止**修改 `internal/protocol/schema/001_*.sql` 至 `029_*.sql` 任何现有文件
- **禁止**在 `pkg/swarm/agents/` 中引入 `pkg/cognition/kernel`（单向依赖，防循环）
- **禁止**在新增代码中使用裸 `error`（统一用 `internal/errors`）
- **禁止**在 `pkg/` 中声明包级别可变变量（用 `sync.Mutex` 保护或 `atomic` 替代）
- **禁止**将 `GovernanceAgent` 的 `PolicyGate` 逻辑与现有 `pkg/action/code_act.go` 的 `PolicyGate` 调用重复——`GovernanceAgent` 是包装层，不是替换层
