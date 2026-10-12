# ADR-0048: M9 自进化引擎生产接线合集

> 状态：Accepted ｜ 首次：2026-07-14 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0049/0054/0055/0056/0058 ｜ 模块：M09/M12 §9

## 决策一：ContinuousSamplingMonitor 写侧接入

M12 §9 读侧完整、写侧 `RecordSample` 原零生产调用。新增 `MaybeSampleAndScore`，接入全部 4 条生产回复路径（SSE / webhook / cron / workflow，共享 `ChatHandler.SampleAndScoreReply`），按 `samplingRate=1%` 异步（独立 goroutine + `context.Background()`）触发 LLM Judge，输出连续 `[0,1]` 分数。Provider 复用 `PickProvider("default")→("general")` 兜底。
`onDegradation` 回调保持 nil（`cmd/polaris/boot_agent.go`）：M9 autoRollback 依赖真实免疫网关，不强接伪回调制造假闭环。
已征询用户：选"全量实现"，接受持续 LLM 成本与对话内容流向评判模型的隐私面。

## 决策二：SessionID 根因修复（原 ADR-0049）

`NewAgent` 构造 `fsm.StateContext` 时 `SessionID` 恒空，致 `events:session:{id}:` 前缀查询、founding_anchor 漂移检测、记忆巩固、PII 快照静默降级。修复为 `StateContext{SessionID: id}`（与 `AgentID` 同源），是 founding_anchor 生产接线的前置条件。

## 决策三：DriftDetector 漂移响应编排（原 ADR-0054）

链路：`Search()` 采样 anchor → `DetectByTaskType()`（按 task_type 评分，<5 样本组跳过）→ `DriftDowngradeRegistry` → `Search()` 读降级状态置零 VectorWeight → 触发 `OnlineReindexer`。`DriftOrchestrator` 周期 168h，覆盖式重评分，不臆测"重嵌已完成"。
分层：`internal/memory`（L1）不得 import `internal/learning/surprise`（L2），改消费方接口（`DriftAnchorRecorder`/`DriftGate`），`cmd/polaris` 组合根注入。
范围外：`EmbeddingVersionTracker.Update`（跨版本分数归一化）依赖的版本标签在 `protocol.CognitiveSearcher` 不存在，已由 [ADR-0062](./ADR-0062-deadcode-final-settlement.md) 接受删除（2026-10 代码中已无该符号）。

## 决策四：`/steer` 激活引导命令面（原 ADR-0055）

`SteeringAdapter.SteerActivations`/`ClearSteering` 接入 `SlashCommandRouter.Dispatch`，实现 `/steer list|import <label> <file>|set <label> <weight>|deactivate|delete`；`ControlVectorStore` 为进程内 label→向量注册表（无持久化，重启需重新 import）。默认 `layer=15`（M09 §1.3）。
不做：`calibrate-layer`（Eval Harness 无按 layer 分组的 case 类型）；"成功率<0.1 自动停用"（无结果归因信号）。

## 决策五：QLoRA/PRM 训练样本采集（原 ADR-0056）

只复用仓库已有信号，不发明 reward 语义：
- **QLoRA**：`ReflexionEngine.replaySuccess`——经 replan 才成功的轨迹（`Success && replanCount>0`），Prompt=成功前上下文，Completion=最终成功步骤结果
- **PRM**：决策一的 LLM Judge `[0,1]` 分数直接作 Reward

共用 `TrainingSampleCollector`（L0 `internal/llm/adapter`，线程安全，达 `batchSize` 经 `SafeGo` 异步 `Train`，失败只记日志不重试不回填）。`batchSize` 默认 **64**（保守初值，供校准）。

## 决策六：SICCleaner LLM 检测器接线（原 ADR-0058）

`AutoCurriculumGenerator.passSafetyAudit` 改用方法 `sicCleaner()` 现场判断（`llmProvider` 由 boot 异步注入）：就绪则 `NewSICCleanerWithDetector(sicDetectFn)`，否则回退 Tier0 正则。`sicDetectFn`（是否试图覆盖/提取系统指令）与 `llmJudgeSafe`（任务描述是否有害）关注点不同，保留两次独立 LLM 调用；失败 fail-closed。

## 决策七：IdleEvolutionScheduler 上下文隔离与主动打断（GD-14-001）

1. 每次触发空闲任务分配独立子 `context.Context` 并记录 cancel
2. 下次 tick 检测到 `InFlight() > 0` 立即取消后台任务
3. `ResourceGovernor.OnActivity` 回调随 `Admit/AdmitLLM` 刷新 `lastActivityAt`
4. 指标 `idle_evolution_tasks_total`

## 反例守护

- 排查"founding_anchor 查不到数据"先确认 `SessionID` 非空，不默认读侧有 Bug
- `sicDetectFn` 与 `llmJudgeSafe` 合并为一次 LLM 调用 — 模糊单一职责
- 调高抽样率前须重评 LLM 成本与隐私面

## 重新评估触发条件

- `onDegradation` 接入 autoRollback 前须有真实免疫网关，不得接伪回调
- 1% 采样率、64 批次均为保守初值，实测后可校准；调高抽样率须重评成本与隐私
- 重建 `EmbeddingVersionTracker` 前须先在 `protocol.CognitiveSearcher` 补版本标签接口

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-07-14~07-22 | 决策一~六（0048 原文/0049/0054/0055/0056/0058） |
| 2026-07-28 | 合并 |
| 2026-08-10 | 采样监控接 7 天基线快照源（`InjectSnapshotStore`） |
| GD-14-001 | 决策七 |

## 引用代码

`internal/eval/analysis/{sampling_monitor,sampling_scorer}.go`、`internal/agent/agent.go`、`cmd/polaris/boot_events.go`、`cmd/polaris/boot_agent.go`、`internal/learning/surprise/{drift_detector,drift_downgrade_registry,drift_orchestrator}.go`、`internal/memory/retrieval/retriever.go`、`internal/llm/adapter/{control_vector_store,training_sample_collector}.go`、`internal/gateway/server/chat/slash_command_steer.go`、`internal/learning/reflexion/reflexion.go`、`internal/learning/curriculum/{curriculum,curriculum_scheduler}.go`、`internal/automation/{idle_evolution,resource_governor}.go`
