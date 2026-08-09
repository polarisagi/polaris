# ADR-0025: 全局架构审查与系统加固综合档案（含原 ADR-0027/0028/0029/0038）

- **状态**: Accepted | **日期**: 2026-06-14~06-25（合并 2026-07-09/2026-07-28）| **模块**: 全仓库

## 决策一：全局审查缺陷修复（原决策，含原 ADR-0027/0028）

多子智能体全局审查（2026-06）对照 M01-M13 与 00-Global-Dictionary 核验出 17 项属实缺陷（1 项失实），按严重度分三档修复，不引入新抽象：

- **P0 安全/数据完整性**：SurrealDB FFI 写函数错误码真实回传；MCP 命令改写补 PolicyGate 鉴权；Wasmtime Store 每次新建避免内存无界累积。
- **P1 逻辑/竞态**：`ToolResult.Suspended` 修正误触发；沙箱等级判定对齐 M07 规则；`ResumeFromSuspended` 补齐；Reaper 缩小锁范围；`extension_instances` 消除双写；`ExecuteTool` 补 JIT Token + DryRun。
- **P2 健壮性**：Cedar 评估 ctx 贯穿防死锁泄漏；Cedar 降级告警；Linux 内存探针改用 `MemAvailable`；`Reaper.Phase2` 改用 `reaper.Run`。
- **核验更正**：M05 三区组装并非"全部缺失"，真实缺陷是外部检索数据无 Spotlighting 围栏——已统一到 `kernel.PromptBuilder`。

**Phase 0 缺口（原 ADR-0028）**：Scheduler 防抖接入 `BackgroundPermit`；FSM/Cedar evaluate goroutine 改用 `concurrent.SafeGo`；`SurpriseCalculator` 接入主路径替代 `ComputeBasic`。

**Gemini 执行缺口（原 ADR-0027）**：LAM 注入真实 `LAMPolicyChecker`（deny-by-default）；`ResourceBudget` 零值替换为真实三维门控；m9-bb-bridge SafeGo 化；`GetEntity` 补 `taint_level` 列绑定修复 XR-16。

## 决策二：Phase 1-2 系统加固（原 ADR-0029，含原 ADR-0038）

- **E — AgentPool**：`internal/agent/pool.go` 实现 per-session Agent（`sync.Map`+信号量，容量见 `state.yaml §thresholds`），替代全服务器单例共享 `sCtx` 的并发覆盖问题。Acquire 超时 100ms 拒绝；Idle 超 10 分钟 GC 回收（回收时须调用 `agent.Shutdown()` 防 goroutine 泄漏）。
- **F — VFS 墓碑**：工作区删除改为原子 `os.Rename` 到墓碑路径+异步 GC，替代直接 `os.RemoveAll`（防僵尸 fd）；关键文件读取新增 `safeOpen`（`O_NOFOLLOW` 防符号链接绕过）。
- **G — SQL Fitness 评估器**：M9 自进化课程样本前置 SQL 预筛（7 天窗口 `fitness = 成功率×(1-平均预测误差)`，<0.5 且样本≥5 直接拒绝不调 LLM），降低 LLM-as-Judge 调用成本。
- **H — SafeGo 全量迁移**：`embedding_batcher`/`channel/adapter/`/`planner/pool.go` 等中高风险裸 goroutine 迁移至 `pkg/concurrent.SafeGo`，防 panic 导致功能静默失效。
- **I — OS Fault 注入 SurpriseIndex**：`InjectFaultSignal(severity)` 加法累积（上限 1.0），沙箱权限拒绝注入 0.8，符号链接越狱注入 0.5。
- **J — 沙箱 Fail-Closed**：移除隔离组件缺失时降级为原生 `exec.Command` 的敞口，NetworkBlock 请求直接拒绝启动；全量迁移 Rust V2 沙箱 `argv` 模式；CodeAct 与核心工具污点级别由引擎层强制覆写为 `TaintHigh`，不采信工具侧上报。
- **K — ShadowExecutor（原 ADR-0038）**：M9 ProgressiveRollout Gate 1 采用异步 EventLog 回放（非实时流量镜像）：定期从 `events` 表采样历史 `llm_call`，注入候选参数推理；副作用工具经 `032_mock_response_cache` 拦截，未命中直接跳过样本，保证零副作用。

## 反例守护

拒绝 eBPF 沙箱探针（`CAP_BPF` 依赖与 Tier-0 目标冲突）；拒绝完整 HLC 时钟（单节点无需求）。

## 引用代码

`internal/agent/pool.go`、`internal/learning/curriculum/`、`pkg/concurrent/safe_go.go`、`internal/eval/analysis/shadow_executor.go`（原 `internal/learning/synthetic/` 已迁移至 `internal/eval/analysis/`）

## 引用

实施细节内联于提交记录，专项整改见 `docs/upgrade/upgrade-01~05`。

> 2026-08-09 追记：重新评估触发条件——本 ADR 是历史缺陷修复档案，条目本身不
> 需要"重新评估"（缺陷已修复即成立）；唯一可能重议的是决策一"反例守护"两项
> （eBPF 沙箱探针、完整 HLC 时钟）——若 Tier-0 硬约束被放宽，或出现真实多节点
> 需求，才重新评估这两项拒绝是否仍然成立。
