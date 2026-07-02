# ADR-0029: Phase 1-2 系统加固（AgentPool / VFS 墓碑 / SQL Fitness / SafeGo 全量 / OS Fault 注入）

**状态**: 已接受 (Accepted)
**日期**: 2026-06-25

## 背景

在 ADR-0028（Phase 0）修复功能正确性缺陷后，Phase 1-2 针对稳定性、安全性和运营成本做系统加固。所有项均来自代码核验，无纯理论推导。

## 决策

### E — AgentPool（per-session Agent 实例）

**现状**：`ChatHandler.Agent` 为全服务器单例，并发请求共享 `sCtx` 字段，多标签页并发操作会互相覆盖 FSM 状态。

**方案**：
- `AgentPool` 接口在 `internal/gateway/server/chat/`（consumer-side）定义。
- `internal/agent/pool.go` 实现：`sync.Map` + 信号量（`chan struct{}`，cap = `TierParams.MaxConcurrentAgents`）。
- Acquire 超时 100ms → `apperr.CodeResourceExhausted`（拒绝而非等待）。
- Idle 超过 10 分钟的 session 由低频 GC ticker 回收（`Pool.GC()`，每 5 分钟调用一次）。
- 容量由 `TierParameters.MaxAgents` 注入，权威值见 `docs/arch/spec/state.yaml §thresholds`：
  - Tier-0 服务器 HT0（`max_agents_server_ht0`）= **3**
  - Tier-0 桌面 HT0（`max_agents_desktop_ht0`）= **2**
  - Tier-1 = **16**
- `configs/defaults.toml` 中 `max_agents = 3` 为服务器 HT0 默认值，可按部署环境覆盖。

**不采纳**：每次请求新建 Agent（初始化成本、SurpriseCalc 冷启动）；无状态 Agent + 每次从 DB 加载 session 状态（改动量过大）。

### F — VFS 墓碑机制 + O_NOFOLLOW

**现状**：`workspace_manager.go:159` 直接 `os.RemoveAll(dir)`，外部进程持有 fd 时产生僵尸；路径前缀检查可被符号链接绕过。

**方案**：
- 工作区删除改为原子 `os.Rename(dir, dir+"_tombstone_"+timestamp)`，异步 GC goroutine（SafeGo 包装，通过 `chan string` 队列）执行实际 `RemoveAll`。
- 关键文件读取路径新增 `safeOpen`（build-tagged `vfs_unix.go`），使用 `unix.Openat(O_RDONLY|O_NOFOLLOW)`，`ELOOP` 错误转为 `CodeForbidden`。
- Windows 平台：`vfs_other.go` 回退到 `os.Open`（无 `O_NOFOLLOW` 等价物）。

**DDL 变更**：无（VFS 路径元数据在内存中管理）。

### G — SQL Fitness 评估器（M9 自进化成本下降）

**现状**：`learning/curriculum/curriculum.go` 的 `isSafe` 在 Tier1+ 调用 LLM-as-Judge 审查所有课程样本，成本随运行时间线性增长。

**方案**：新增 `SQLFitnessEvaluator`（`learning/curriculum/fitness.go`），在 `isSafe` 入口前置 SQL 预筛：
- 查询 `events` 表（`type='tool_execute'`，`skill_id=?`，7 天窗口）。
- 计算 `fitness = 成功率 × (1 - 平均预测误差)`。
- `fitness < 0.5` 且样本 ≥ 5 → 直接拒绝，不调用 LLM。
- 样本 < 5 或 `fitness >= 0.5` → 交由原 `isSafe` LLM 路径。

**适用范围**：仅针对"有历史记录的技能"做预筛，对新技能无影响。`SQLFitnessEvaluator` nil-safe，不注入时完全退回原行为。

**不采纳**：完全替换 LLM-as-Judge（安全审查需要语义理解，SQL 无法替代）；在 Eval Harness 层做（职责属 M9 Curriculum，不属于 M12）。

### H — SafeGo 全量迁移（中高风险裸 goroutine）

**现状**：`pkg/concurrent.SafeGo` 已实现（ADR-0027 BUG-3），但以下路径未迁移：`store/search/embedding_batcher.go`、`eval/analysis/shadow_executor.go`、`channel/adapter/` 各平台 poll goroutine、`swarm/planner/pool.go`。

**优先级**：`embedding_batcher`（panic → 向量检索静默降级为纯 BM25）和 `channel/adapter/`（panic → 平台静默下线）最高；`shadow_executor` 和 `planner/pool` 其次。

**LLM 事件写入 goroutine**（`llm/router.go` 独立 ctx2 的 200ms goroutine）：P3，后续通过 `store/audit/event_buffer.go` 批处理替代，本轮不处理。

### I — OS Fault → SurpriseIndex 注入

**现状**：沙箱权限拒绝、符号链接越狱等 OS 事件静默失败，Agent 无从感知越权行为。

**方案**：
- `metrics.SurpriseIndex` 的 `InjectFaultSignal(severity float64)` 采用**加法累积**（`lastValue += severity`），上限 1.0，防止溢出。实现比规划阶段更简单（无 EMA 混合），与 SurpriseIndex 主路径的 EMA 计算互不干扰。
- 严重度按事件类型分级：
  - `sandbox/` Execute 的 `fs.ErrPermission` 分支：`InjectFaultSignal(0.8)`（高严重度——沙箱权限拒绝为越权尝试信号）
  - `vfs/safeOpen` 的 ELOOP（符号链接越狱）分支：`InjectFaultSignal(0.5)`（中严重度——路径越界尝试）
- 分级注入使路由系统对不同类型 OS 安全事件的敏感度有所区分，而非统一对待。
- 无 eBPF 依赖，无 root 要求，跨平台可用。

**为什么不用 eBPF**：需要 `CAP_BPF`（Linux 5.8+），macOS 不可用，Docker 默认不开启；与 Tier-0 "2GB VPS 可运行"目标冲突。OS 层错误在 Go 侧已可捕获，直接注入等效且更简单。

### J — 沙箱安全加固与 Fail-Closed 机制

**现状**：
- 早期沙箱机制在底层沙箱组件（如 bubblewrap / seatbelt）缺失时，默认降级为原生 `exec.Command`（静默裸跑），在缺乏特权隔离的容器或 CI 环境中形成安全敞口。
- 原有的 Go 侧 CmdRunner V1 实现包含大量冗余的命名空间与环境注入代码。
- MCP 客户端与部分内置执行工具（如 git, ffmpeg, tts）直接依赖原生子进程执行，未纳入全局沙箱管控。
- 审计日志中污点级别（Taint Level）由工具侧自行上报，存在被伪造的风险。

**方案**：
- **强制 Fail-Closed 原则**：移除原生执行降级路径。当工具或扩展请求网络隔离（NetworkBlock）但底层安全隔离组件不可用时，直接返回失败拒绝启动，杜绝静默裸跑。
- **全量迁移 Rust V2 沙箱**：删除 Go 侧 V1 CmdRunner 死代码。统一所有进程级沙箱调用（含 CmdRunner、MCP Client 等）至 Rust V2 的 `argv` 执行模式，从根源上杜绝 Shell 注入漏洞。
- **内置工具沙箱化**：为 git, ffmpeg, tts 等内置进程工具封装沙箱执行上下文（`runSandboxedArgv`）。极少数必须豁免沙箱的组件（如音视频二进制流处理、长驻后台进程服务）必须在源码层面添加豁免白名单与原因说明。
- **统一高污点审计**：CodeAct 与核心工具执行产生事件日志时，引擎层强制覆写其污点级别为 `TaintHigh`，不再采信工具侧动态传递的等级。

**适用范围**：涵盖整个 M07 模块下的子进程启动逻辑，包括 `internal/sandbox`、内置工具集以及 MCP 客户端连接。

## 不采纳方案汇总

| 方案 | 原因 |
|------|------|
| Spectre 时钟粗化（10ms 精度） | Spectre 需本地代码执行权，WASM 已限制；10ms 精度破坏 timer/ULID |
| 全局 Mlock/memclr 凭证锁定 | 单用户本地场景无紧迫性；多租户时作 P0 追加 |
| 完整 HLC 混合逻辑时钟 | 单节点无跨节点时钟协调需求；`ulid.Monotonic()` 读取器足够（P3） |
| eBPF 沙箱探针 | 见 §I |

## 后果

- **正面**：并发 session 状态隔离；工作区删除安全（无僵尸 fd）；M9 自进化 API 成本下降；中高风险 goroutine panic 不再导致功能静默失效；Agent 可感知 OS 级越权事件并调整路由。
- **负面**：AgentPool 增加 pool 管理复杂度（GC ticker、容量限制）；VFS GC 异步化引入极小的清理延迟（秒级）。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-25 | 初稿，Accepted |
