# ADR-0056: QLoRA/PRM 训练样本采集 + 批次触发流水线

- **状态**: Accepted（已执行）
- **日期**: 2026-07-21
- **决策者**: MrLaoLiAI
- **相关模块**: `internal/llm/adapter/`、`internal/learning/reflexion/`、
  `internal/eval/analysis/`、`cmd/polaris/boot_agent.go`、
  `internal/config/thresholds.go`

## 上下文

承接 ADR-0052 DEFER 条目：`adapter.QLoRAAdapter.Train`/`PRMAdapter.Train`/
`postJSON` 与 SteeringAdapter 同一模式——适配器骨架 + Tier 门控完整，但"缺
M9 curriculum/reflexion 产出训练样本并决定触发时机的上游流水线"。用户对本
任务的决策："我先给出合理默认值草案再实现，你后续校准"。

先用一个 subagent 调研全仓库，确认结论：**这不是一次简单桥接**。
`docs/arch/M09-Self-Improvement-Engine.md` §4"条件梯度训练"只定义了 Tier
门控（`FeatureQLoRA`/`FeaturePRMTraining`）与适配器请求体形状，**未给出任何
触发条件、批次大小、样本来源规范**；全仓库搜索确认没有任何地方存储结构化
的 `(prompt, completion)` 训练样本或 per-turn reward 标签。与 Task #6
（`/steer`，文档给出了精确命令面 + 默认层号）性质不同——这里连"要接的东西
是什么形状"都没有规定。

## 决策

拒绝凭空发明"什么是训练样本""什么是 reward"，改为**只复用仓库里已经真实
存在、语义明确的信号**，把"格式转换 + 批次累积触发"作为本次新增的、清楚
标注的设计决策：

### QLoRA 样本来源：ReflexionEngine.replaySuccess

`internal/learning/reflexion/reflexion.go` 的 `Reflect()` 已有明确、既有的
真实触发条件：`result.Success && replanCount > 0 && len(trajectory) > 0`——
"经过 replan 才成功"的纠偏轨迹，AgentHER 设计本就认为这是值得强化的高质量
数据。新增 `buildQLoRASample(trajectory []learning.Step)`（`reflexion.go`）：

- Prompt = 最终成功步骤之前的完整轨迹上下文（`formatTrajectory` 已有函数，
  犯错→探索过程），单步即成功时退化为用该步骤的 `Action`。
- Completion = 最终成功步骤的 `Result`。

这一 Prompt/Completion 切分规则是本次新增的显式设计决策（文档未规定），
但转换的输入数据本身（trajectory 的 Action/Result 字符串）完全真实，非
臆测业务语义。

### PRM 样本来源：M12 §9 生产流量采样 LLM Judge 打分

`internal/eval/analysis/sampling_scorer.go` 的 `judgeReplyQuality` 已对 1%
生产流量的 (query, response) 打出 `[0,1]` 连续质量分（ADR-0048 已实现，
原用于 `ContinuousSamplingMonitor` 退化检测）。这个分数就是一个真实存在、
含义明确的质量信号，直接复用作 PRM 的 Reward 标签：
`TrainingSample{Prompt: query, Completion: response, Reward: score}`。

### 共用的批次累积触发器

`internal/llm/adapter/training_sample_collector.go`（新增）：
`TrainingSampleCollector`，线程安全累积 `TrainingSample`，达到 `batchSize`
时经 `concurrent.SafeGo` 异步调用 `TrainingAdapter.Train`（`QLoRAAdapter`/
`PRMAdapter` 已共用同一接口，见 `training.go`），随后清空缓冲区。失败只记
日志，不重试、不回填——训练服务不可用时优先丢弃样本而非阻塞生产路径或无界
增长内存。

放在 `internal/llm/adapter`（L0）而非 `internal/learning`（L2）：`learning`
（L2）与 `eval`（L3）都能合法 import `llm/adapter`（L0），共用同一采集器类型
避免重复实现（HE-3 不新造重复类型）。

### 接线点

- `ReflexionEngine.qloraCollector`（`InjectQLoRACollector` 注入，可选/nil-safe）
- `ContinuousSamplingMonitor.prmCollector`（`InjectPRMCollector` 注入，可选/nil-safe）
- `cmd/polaris/boot_agent.go`：`sb.QLoRA != nil`/`sb.PRM != nil` 时才构造
  `TrainingSampleCollector` 并注入（刻意避免"nil 具体类型包进非 nil
  interface"陷阱——`sb.QLoRA`/`sb.PRM` 为 `*QLoRAAdapter`/`*PRMAdapter` 具体
  指针类型，无条件传入会让 `TrainingAdapter` interface 本身非 nil，
  `TrainingSampleCollector` 的 nil 判断会失效）。

### 提议的默认值（用户已授权）

`M9SelfImproveThresholds.QLoRATrainBatchSize`/`PRMTrainBatchSize` 均默认
**64**：LoRA/PRM 小批次微调的常见量级下限。未采用不同数值的原因：两条数据
源本身都是低频事件（QLoRA 样本依赖"经 replan 才成功"，PRM 样本依赖 1% 采样
命中且需要 provider 非 nil），批次大小定得更大只会让训练更久不触发，定得
更小则失去"批次"训练的意义；64 是两者之间简单可行的折衷起点，非精确调优
结果，供用户后续按真实数据速率校准。

## 判断依据

延续 R1：只对"真实数据源 + 真实触发条件已存在，只缺格式转换和批次编排"的
部分做实现（QLoRA 的 replan 成功轨迹、PRM 的 Judge 打分）。没有为
`calibrate-layer` 式的"完全无支撑机制"的功能编造实现——本任务不存在这类
子项，两条数据源都有真实基础,故本次是完整实现而非部分实现+标注延期。

## 后果

- **正向**：`go build`/`go test ./... -race`（受影响包）/`golangci-lint`/
  `make gen-threshold-examples` 全绿；`deadcode` 确认
  `QLoRAAdapter.Train`/`PRMAdapter.Train`/`postJSON` 不再出现；新增
  `internal/llm/adapter/training_sample_collector_test.go`、
  `internal/learning/reflexion/reflexion_qlora_test.go`、
  `internal/eval/analysis/sampling_scorer_prm_test.go`。
- **负向**：QLoRA/PRM 样本天然低频（分别依赖"经 replan 成功"事件与 1% 采样
  命中），实际达到 64 条批次阈值可能需要较长运行时间——这是"后台慢速自
  演化"特性本身，非实现缺陷；批次大小默认值待用户按真实数据速率校准。

## 引用代码

- `internal/llm/adapter/training_sample_collector.go`（新增）
- `internal/llm/adapter/training.go`（原有 `TrainingAdapter`/`QLoRAAdapter`/`PRMAdapter`，本次未改动其内部逻辑）
- `internal/learning/reflexion/reflexion.go`（`buildQLoRASample`/`InjectQLoRACollector`）
- `internal/eval/analysis/sampling_scorer.go`/`sampling_monitor.go`（`InjectPRMCollector`）
- `cmd/polaris/boot_agent.go`（构造 + 注入）
- `internal/config/thresholds.go`（`QLoRATrainBatchSize`/`PRMTrainBatchSize`）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-21 | 初稿：QLoRA（reflexion 纠偏轨迹）+ PRM（M12 §9 Judge 打分）样本采集与批次触发接线完成 |
