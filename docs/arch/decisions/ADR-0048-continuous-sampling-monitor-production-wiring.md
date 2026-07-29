# ADR-0048: M9 自进化引擎生产接线合集（含原 ADR-0049/0054/0055/0056/0058）

- **状态**: Accepted（已执行）| **日期**: 2026-07-14~07-22（合并 2026-07-28）| **模块**: M09/M12 §9

## 决策一：ContinuousSamplingMonitor 写侧接入（原决策）

M12 §9 连续采样监控读侧完整、写侧 `RecordSample` 此前全仓零生产调用点。新增 `MaybeSampleAndScore`，接入全部 4 条生产 assistant 回复路径（交互式 SSE / webhook / cron / workflow，共享 `ChatHandler.SampleAndScoreReply`），按 1% 概率异步（独立 goroutine + `context.Background()`）触发 LLM Judge 打分，输出连续 `[0,1]` 分数（非结构化 pass/fail）。判分 Provider 复用 `PickProvider("default")→("general")` 兜底链。`onDegradation` 回调保持 nil——M9 autoRollback 依赖的真实免疫网关（`boot_immune.go`）仍是占位，不强行接伪回调制造假闭环。

**已征询用户**：在"全量实现/仅主路径/启发式替代/暂不接入"四选项中选择全量实现，接受持续 LLM 调用成本与用户对话内容流向评判模型的隐私面。

## 决策二：SessionID 根因修复（原 ADR-0049）

`NewAgent` 构造 `fsm.StateContext` 时 `SessionID` 从未赋值（恒空字符串），导致 `events:session:{id}:` 前缀查询、founding_anchor 漂移检测、记忆巩固触发、PII 快照等多处消费方静默降级/跳过。修复：`StateContext{SessionID: id}`，与 `AgentID` 同源。这是 founding_anchor 生产接线的前置条件——读侧逻辑再完整也建立在空数据上。

## 决策三：DriftDetector 漂移响应编排器接线（原 ADR-0054）

补齐漂移检测编排链：`Search() 采样 anchor → DetectByTaskType() 按 task_type 评分 → DriftDowngradeRegistry 同步降级状态 → Search() 读降级状态零权重 VectorWeight → 触发 OnlineReindexer 批次`。新增 `DetectByTaskType`（按 task_type 分组评分，<5 样本组跳过）、`DriftDowngradeRegistry`、`DriftOrchestrator`（周期 168h，覆盖式重新评分，不臆测"重嵌已完成"）。

**分层修正**：`internal/memory`（L1）不得直接 import `internal/learning/surprise`（L2）——改为消费方接口模式（`DriftAnchorRecorder`/`DriftGate`），`cmd/polaris` 组合根注入具体实例。

**范围订正**：`EmbeddingVersionTracker.Update`（跨 embedding 版本混合检索分数归一化）不在本次范围——依赖的版本标签在 `protocol.CognitiveSearcher` 接口中不存在，属独立架构变更（后于 [ADR-0062](./ADR-0062-deadcode-final-settlement.md) 接受删除，待未来立项重建）。

## 决策四：`/steer` 激活引导命令面接线（原 ADR-0055）

`SteeringAdapter.SteerActivations`/`ClearSteering` 适配器骨架已完整，接入现成的 `SlashCommandRouter.Dispatch`（与 `/context`/`/compact` 同一模式），实现 `/steer list|import <label> <file>|set <label> <weight>|deactivate|delete`。新增 `ControlVectorStore`（进程内存 label→向量注册表）。默认注入层 `layer=15`（采用 M09 §1.3 文档已定义值，非臆测）。

**不在本次范围**（诚实标注，非臆测实现）：`calibrate-layer <task_type>` 需要按 layer 分组的评估轮次机制（Eval Harness 无此 case 类型）；"成功率<0.1 自动停用"需要结果归因信号（不存在）；`ControlVectorStore` 无持久化，重启需重新 import。

## 决策五：QLoRA/PRM 训练样本采集流水线（原 ADR-0056）

拒绝凭空发明训练样本/reward 语义，只复用仓库中已真实存在的信号：

- **QLoRA 样本源**：`ReflexionEngine.replaySuccess`——"经 replan 才成功"的纠偏轨迹（`result.Success && replanCount>0`），Prompt=成功前轨迹上下文，Completion=最终成功步骤结果。
- **PRM 样本源**：M12 §9 生产流量采样 LLM Judge 打分（决策一），`[0,1]` 连续分数直接作为 Reward 标签。

共用 `TrainingSampleCollector`（L0 `internal/llm/adapter`，线程安全累积，达 `batchSize` 时经 `SafeGo` 异步调 `Train`，失败只记日志不重试不回填）。批次大小默认值 **64**（LoRA/PRM 小批次微调常见量级，两条数据源均低频，非精确调优结果，供后续校准）。

## 决策六：SICCleaner LLM 检测器接线（原 ADR-0058）

`AutoCurriculumGenerator.passSafetyAudit` 此前固定用 `guard.NewSICCleaner()`（内置正则），未升级到 M11 §2.2 设计的 LLM 感知检测器。改为方法 `sicCleaner()` 现场判断（非构造时固定绑定，因 `llmProvider` 由 boot 阶段异步注入）：`llmProvider` 就绪则 `NewSICCleanerWithDetector(sicDetectFn)`，否则回退 Tier0 正则。

`sicDetectFn`（判断"是否试图覆盖/提取系统指令"，prompt injection）与既有 `llmJudgeSafe`（判断"任务描述本身是否有害"）关注点不同，保留两次独立 LLM 调用，不合并——合并会让单一职责契约模糊。失败 fail-closed，与 `llmJudgeSafe` 一致。

## 决策七：IdleEvolutionScheduler 上下文隔离与主动打断机制（GD-14-001）

`IdleEvolutionScheduler`（Tier0 睡眠期任务，包括记忆巴固与滤波）此前仅实现了结构骨架，缺乏任务上下文生命周期管理。现补充：
1. 每次判定空闲并触发任务时，分配一个独立的子 `context.Context` 并记录 cancel 引用。
2. 在下一次 tick 扫描时若检测到有交互流量打断（`InFlight() > 0`），立刻取消这些后台任务，避免与前台抢占资源。
3. `ResourceGovernor` 暴露 `OnActivity` 回调给 `Admit/AdmitLLM`，实现每次准入精确刷新最后活跃时间（`lastActivityAt`）。
4. 补充 `idle_evolution_tasks_total` Prometheus 指标埋点。

## 反例守护

调高抽样率前须重新评估 LLM 成本与隐私面。排查"founding_anchor 查不到数据"类问题时先确认 `SessionID` 非空，而非默认假设读侧有 Bug。拒绝 `sicDetectFn` 与 `llmJudgeSafe` 合并为一次 LLM 调用。

## 引用代码

`internal/eval/analysis/sampling_scorer.go`、`internal/agent/agent.go`（`NewAgent`）、`cmd/polaris/boot_events.go`、`internal/learning/surprise/{drift_detector,drift_downgrade_registry,drift_orchestrator}.go`、`internal/memory/retrieval/retriever.go`、`internal/llm/adapter/{control_vector_store,training_sample_collector}.go`、`internal/gateway/server/chat/slash_command_steer.go`、`internal/learning/reflexion/reflexion.go`、`internal/learning/curriculum/{curriculum,curriculum_scheduler}.go`、`internal/automation/idle_evolution.go`、`internal/automation/resource_governor.go`、`cmd/polaris/boot_agent.go`
