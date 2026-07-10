# ADR-0029: Phase 1-2 系统加固（AgentPool / VFS 墓碑 / SQL Fitness / SafeGo 全量 / OS Fault 注入）

**状态**: 已接受 (Accepted)
**日期**: 2026-06-25

## 背景

在 ADR-0028（Phase 0）修复功能正确性缺陷后，Phase 1-2 针对稳定性、安全性和运营成本做系统加固。所有项均来自代码核验，无纯理论推导。

## 决策

### E — AgentPool（per-session Agent 实例）

**问题**：`ChatHandler.Agent` 为全服务器单例，并发请求共享 `sCtx` 字段，多标签页并发操作会互相覆盖 FSM 状态。

**决策**：`internal/agent/pool.go` 实现 `AgentPool`（`sync.Map` + 信号量，cap = `TierParams.MaxConcurrentAgents`）；Acquire 超时 100ms 拒绝而非等待；Idle 超 10 分钟由 GC ticker 回收。容量权威值见 `docs/arch/spec/state.yaml §thresholds`（Tier-0 服务器=3，Tier-0 桌面=2，Tier-1=16）。

**不采纳**：每次请求新建 Agent（初始化成本、SurpriseCalc 冷启动）；无状态 Agent + 每次从 DB 加载 session 状态（改动量过大）。

### F — VFS 墓碑机制 + O_NOFOLLOW

**问题**：工作区删除直接 `os.RemoveAll(dir)`，外部进程持有 fd 时产生僵尸；路径前缀检查可被符号链接绕过。

**决策**：删除改为原子 `os.Rename` 到墓碑路径，异步 GC 执行实际 `RemoveAll`；关键文件读取路径新增 `safeOpen`（`unix.Openat(O_RDONLY|O_NOFOLLOW)`，`ELOOP` 转 `CodeForbidden`），Windows 无等价物回退到 `os.Open`。

### G — SQL Fitness 评估器（M9 自进化成本下降）

**问题**：`isSafe` 在 Tier1+ 调用 LLM-as-Judge 审查所有课程样本，成本随运行时间线性增长。

**决策**：新增 `SQLFitnessEvaluator` 前置 SQL 预筛（`events` 表 7 天窗口计算 `fitness = 成功率 × (1-平均预测误差)`，`fitness<0.5` 且样本≥5 直接拒绝不调 LLM），仅对"有历史记录的技能"生效，nil-safe。

**不采纳**：完全替换 LLM-as-Judge（安全审查需要语义理解，SQL 无法替代）；放在 Eval Harness 层做（职责属 M9 Curriculum，不属于 M12）。

### H — SafeGo 全量迁移（中高风险裸 goroutine）

**问题**：`pkg/concurrent.SafeGo`（ADR-0027 BUG-3）已实现，但 `embedding_batcher.go`、`shadow_executor.go`、`channel/adapter/` 各平台 poll goroutine、`planner/pool.go` 未迁移。

**决策**：按优先级迁移——`embedding_batcher`（panic 会使向量检索静默降级为纯 BM25）与 `channel/adapter/`（panic 会使平台静默下线）最高；`shadow_executor`/`planner/pool` 其次。`llm/router.go` 的事件写入 goroutine 定为 P3，后续由 `event_buffer.go` 批处理替代，本轮不处理。（注：后续更新详见 2026-07-08 修订记录）

### I — OS Fault → SurpriseIndex 注入

**问题**：沙箱权限拒绝、符号链接越狱等 OS 事件静默失败，Agent 无从感知越权行为。

**决策**：`InjectFaultSignal(severity)` 加法累积（`lastValue += severity`，上限 1.0），与 SurpriseIndex 主路径 EMA 计算互不干扰；沙箱权限拒绝注入 0.8（高严重度），符号链接越狱注入 0.5（中严重度），使路由对不同 OS 安全事件的敏感度有区分。

**为什么不用 eBPF**：需要 `CAP_BPF`（Linux 5.8+），macOS 不可用，Docker 默认不开启，与 Tier-0 "2GB VPS 可运行"目标冲突；OS 层错误在 Go 侧已可捕获，直接注入等效且更简单。

### J — 沙箱安全加固与 Fail-Closed 机制

**问题**：早期沙箱在 bubblewrap/seatbelt 缺失时默认降级为原生 `exec.Command`（静默裸跑），在无特权隔离的容器/CI 环境形成安全敞口；git/ffmpeg/tts 等内置工具未纳入沙箱管控；污点级别由工具侧自行上报，可被伪造。

**决策**：移除原生执行降级路径，NetworkBlock 请求在隔离组件不可用时直接拒绝启动（Fail-Closed）；全量迁移至 Rust V2 沙箱的 `argv` 执行模式，删除 Go 侧 V1 CmdRunner 死代码，杜绝 Shell 注入；为 git/ffmpeg/tts 等内置工具封装 `runSandboxedArgv`，必须豁免的组件需源码层面白名单+原因说明；CodeAct 与核心工具执行事件日志的污点级别由引擎层强制覆写为 `TaintHigh`，不再采信工具侧上报。适用范围覆盖 M07 全部子进程启动逻辑。

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

### K — 影子执行器设计（ShadowExecutor）

> **来源**：原 ADR-0038（影子执行器设计与异步回放选型），2026-07-09 综合并入本 ADR。

**背景**：M9 ProgressiveRollout Gate 1 被设计为 ShadowExecution，用于在不影响真实用户请求的情况下，使用历史流量评估新提示词、权重的质量。然而早期架构中该环节未实现，导致新版本直接进入 Staging 阶段，缺失关键安全网。

**已评估方案**：

| 方案 | 优点 | 缺点 |
|------|------|------|
| 实时流量镜像（Traffic Mirroring） | 延时真实、数据新鲜 | 与核心链路耦合重；工具调用拦截复杂；单机部署不现实 |
| 异步 EventLog 回放（EventLog Replay） | 核心链路零侵入；可批量处理；天然具备重试能力 | 非实时，存在分钟级延迟 |

**决策**：选择**方案 B（异步 EventLog 回放）**。

在本场景中，评估主要用于质量验证和回归探测，离线计算的分钟级延迟可接受。`ShadowExecutor` 定期从 `events` 表中采样历史 `llm_call` 记录，注入 Candidate 参数进行 LLM 推理。对于副作用工具，使用 `032_mock_response_cache` 拦截，若未命中直接标记样本跳过，保证**零副作用**铁律。

**不采纳**：实时流量镜像（见上表）。

**后果**：ProgressiveRollout 可依赖真实打分机制通过 Gate 1；生产环境用户性能不受测试干扰；系统增加异步读取事件和写 mock 缓存的路径。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-25 | 初稿，Accepted |
| 2026-07-04 | 精简：E~J 各节"现状/方案"叙述收敛为"问题/决策"精简段落，不采纳方案、技术理由（如"为什么不用 eBPF"）与后果保持不变 |
| 2026-07-08 | §H 状态更新（Phase 4 硬依赖校验复核）：`embedding_batcher`、`channel/adapter/`、`planner/pool.go` 均已确认迁移至 `concurrent.SafeGo`。`shadow_executor.go` 对应功能已确认从未接线并删除，本项作废。`llm/router.go` 事件写入 goroutine 随死接口于 2026-07-08 整体删除，无需再包 SafeGo。另发现并补齐了 `cronadmin` 常驻 goroutine 同类缺口。至此 SafeGo 全量迁移的原始清单（含本次新发现项）已无未处理项。详见 `local_playground/reports/phase4-hard-dep-and-deadcode-followup-20260708.md`。 |
| 2026-07-09 | 综合扩展：将原 ADR-0038（影子执行器设计与异步回放选型）内容并入本 ADR §K 节，并废弃原 ADR-0038 文件 |
| 2026-07-10 | §K 状态更新：2026-07-08 "确认未接线并删除"的判断不完整——`shadow_executor.go` 实现完整、测试完整（308+257 行），真正缺失的只是顶层周期触发器；同日再次被误判为死代码删除（第三次同类循环）。经与用户确认后彻底解决：(1) 恢复实现，`//custom-nolint:bare-infer` 换为 `safecall.Infer`；(2) `cmd/polaris/boot_agent.go` 补 5 分钟周期触发器，发现 Gate 2(Shadow) 候选并调用 `RunReplayBatch`；(3) 顺带发现并修复更深的根因——`handleEvalCompleted` 此前 Eval(Gate 1) 一过就同步 `versionStore.Activate`，Gate 2/3 从未真正拦截任何候选；`m9Engine` 此前注入的 `rollout` 是纯内存 `ProgressiveRollout`（无 DB 持久化），与 LogicCollapseMonitor 使用的真实 `SQLiteRolloutStore` 是两份互不相干的状态；`SetStagingPipeline`/`SetVersionStore` 从未在生产启动代码中被调用；`promptOptimizer` 以 `(nil, nil, 0)` 构造。现统一为单一 `SQLiteRolloutStore` 实例，`ConfirmShadow` 通过后经 `promptActivator` 回调激活 Prompt 候选，取代旧的同步 Activate 路径。详见本次会话记录。 |
