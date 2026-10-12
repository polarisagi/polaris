# ADR-0025: 全局架构审查与系统加固综合档案

> 状态：Accepted ｜ 首次：2026-06-14 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0027/0028/0029/0038 ｜ 模块：全仓库

## 决策一：全局审查缺陷修复（原决策，含原 ADR-0027/0028）

2026-06 多子智能体对照 M01-M13 与 00-Global-Dictionary 核验出 17 项属实缺陷（1 项失实），分三档修复，不引入新抽象：
- **P0 安全/数据完整性**：SurrealDB FFI 写函数错误码真实回传；MCP 命令改写补 PolicyGate 鉴权；Wasmtime Store 每次新建。
- **P1 逻辑/竞态**：`ToolResult.Suspended` 误触发；沙箱等级判定对齐 M07；补 `ResumeFromSuspended`；Reaper 缩锁；`extension_instances` 消除双写；`ExecuteTool` 补 JIT Token + DryRun。
- **P2 健壮性**：Cedar 评估 ctx 贯穿防死锁；Cedar 降级告警；Linux 内存探针用 `MemAvailable`；`Reaper.Phase2` 用 `reaper.Run`。
- **核验更正**：M05 三区组装并非全缺，真实缺陷是外部检索数据无 Spotlighting 围栏，已统一到 `kernel.PromptBuilder`。
- **Phase 0 缺口（原 0028）**：Scheduler 防抖接 `BackgroundPermit`；FSM/Cedar goroutine 用 `concurrent.SafeGo`；`SurpriseCalculator` 接主路径替代 `ComputeBasic`。
- **Gemini 执行缺口（原 0027）**：LAM 注入真实 `LAMPolicyChecker`（deny-by-default）；`ResourceBudget` 真实三维门控；m9-bb-bridge SafeGo 化；`GetEntity` 补 `taint_level` 列绑定（XR-16）。

## 决策二：Phase 1-2 系统加固（原 ADR-0029/0038）

- **E AgentPool**：`internal/agent/pool.go` per-session Agent（`sync.Map`+信号量，容量见 `state.yaml §thresholds`）替代单例共享 `sCtx`。Acquire 超时 100ms 拒绝；Idle 超 10 分钟 GC（须调 `agent.Shutdown()`）。后台 `AcquireHeadless`（等待上限 10 分钟）另占容量 `maxSize − agents.interactive_reserved`（下限 1）的后台信号量，至少 1 槽恒为交互保留（2026-09-25，Tier-0 的 4 槽曾被 3 个 headless 任务 + 1 会话占满，交互 Acquire 必然落空）。
- **F VFS 墓碑**：工作区删除改原子 `os.Rename` 到墓碑路径 + 异步 GC；关键文件读取经 `internal/vfs` 的 `SafeOpen`（`O_NOFOLLOW` 防符号链接绕过）。
- **G SQL Fitness 评估器**：M9 课程样本前置 SQL 预筛（7 天 `fitness = 成功率×(1-平均预测误差)`，<0.5 且样本≥5 直接拒绝，不调 LLM）。
- **H SafeGo 全量迁移**：`embedding_batcher`/`channel/adapter/`/`planner/pool.go` 等裸 goroutine → `pkg/concurrent.SafeGo`。
- **I OS Fault 注入 SurpriseIndex**：`InjectFaultSignal(severity)` 加法累积（上限 1.0），沙箱权限拒绝 0.8，符号链接越狱 0.5。
- **J 沙箱 Fail-Closed**：移除隔离缺失时降级原生 `exec.Command` 的敞口，NetworkBlock 请求直接拒绝；全量迁 Rust V2 `argv` 模式；CodeAct 与核心工具污点级由引擎层强制覆写 `TaintHigh`，不采信工具侧上报。
- **K ShadowExecutor（原 0038）**：M9 ProgressiveRollout Gate 1 用异步 EventLog 回放（非实时镜像）：从 `events` 表采样历史 `llm_call` 注入候选参数；副作用工具经 `032_mock_response_cache` 拦截，未命中跳过样本，零副作用。

## 反例守护

- eBPF 沙箱探针 — `CAP_BPF` 依赖与 Tier-0 冲突
- 完整 HLC 时钟 — 单节点无需求

## 重新评估触发条件

本 ADR 为历史缺陷修复档案，条目无需重评。仅当 Tier-0 硬约束放宽或出现真实多节点需求，才重议上述两项拒绝。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-06-14~06-25 | 决策一（含 0027/0028）、决策二（0029/0038） |
| 2026-07-09 | 合并 |
| 2026-07-28 | 合并 ADR-0027/0028/0029/0038 |
| 2026-09-25 | AgentPool 交互保留槽 |

## 引用代码

`internal/agent/pool.go`、`internal/learning/curriculum/`、`pkg/concurrent/safe_go.go`、`internal/vfs/`、`internal/eval/analysis/shadow_executor.go`（原 `internal/learning/synthetic/` 已迁入 `internal/eval/analysis/`）
