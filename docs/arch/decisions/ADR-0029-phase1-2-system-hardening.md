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
- Tier-0：`MaxConcurrentAgents = 4`，Tier-1：16。

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
- `metrics.SurpriseIndex` 新增 `InjectFaultSignal(contribution float64)`，EMA 混合（0.3×新值 + 0.7×历史均值），单次贡献上限 0.5，防止单次事件主导路由决策。
- 在 `vfs/safeOpen`（ELOOP 分支）和 `sandbox/` Execute 的 `fs.ErrPermission` 分支中调用 `InjectFaultSignal(0.3)`。
- 无 eBPF 依赖，无 root 要求，跨平台可用。

**为什么不用 eBPF**：需要 `CAP_BPF`（Linux 5.8+），macOS 不可用，Docker 默认不开启；与 Tier-0 "2GB VPS 可运行"目标冲突。OS 层错误在 Go 侧已可捕获，直接注入等效且更简单。

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
