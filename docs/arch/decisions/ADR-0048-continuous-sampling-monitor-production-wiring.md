# ADR-0048: ContinuousSamplingMonitor 写侧接入生产流量，1% 抽样 + LLM Judge 打分

- **状态**: Accepted（已执行）
- **日期**: 2026-07-14
- **决策者**: MrLaoLiAI（明确确认按文档全量实现，而非用启发式替代或延后）
- **相关模块**: M12 §9（连续采样监控）/ `internal/eval/analysis` / `internal/gateway/server/chat`

## 上下文

M12-Eval-Harness.md §9 设计的连续采样监控（滑动窗口 100 条 + 10min 定时检测 + 退化归因）读侧（`GetL3Threshold`/`Start`/`CheckDegradation`/`InjectL3ThresholdProvider`）已完整实现，但写侧 `RecordSample` 全仓零生产调用点（仅测试调用），`samplingRate=0.01` 是从未被引用的死常量——窗口永远为空，退化检测形同虚设，文档"✅ 已实现"的断言此前具有误导性（见 M12-Eval-Harness.md §9 订正、docs/arch/M12-Eval-Harness.md L165）。

要按文档设计真正补全写侧，唯一可行路径是给生产回复流量接入抽样 + LLM Judge 打分（文档 §9 原文即"共享 LLM Judge 引擎"）。这与此前"把现有死代码接起来"的任务性质不同——它会产生持续的真实 LLM 调用开销、增加异步延迟、且把用户对话内容发给额外的评判模型（隐私面）。因此在动工前明确征询用户意见，在"按文档全量实现 / 仅接入主交互路径 / 用免费启发式替代 LLM Judge / 暂不接入"四个选项中，用户选择了**按文档全量实现**。

## 决策

**新增 `ContinuousSamplingMonitor.MaybeSampleAndScore`，接入全部 4 条生产 assistant 回复路径，按 1% 概率异步触发 LLM Judge 打分。**

依据：

1. 抽样门控在调用方文件层面完成（`samplingRate` 常量首次被引用），命中前零开销直接返回，未命中不产生任何 I/O；命中后在独立 `concurrent.SafeGo` goroutine 中执行，使用 `context.Background()` 而非调用方请求 ctx——HTTP 请求可能在打分完成前已经结束，不应因为打分阻塞或依赖请求生命周期。
2. 判分 Prompt 与既有 `RunnerImpl` Level4LLMJudge / `ShadowExecutor.scoreShadow` 同一 `safecall.Infer` 调用模式，但输出连续 `[0,1]` 分数而非结构化布尔 pass/fail——服务于"退化趋势"场景而非"是否达标"场景，因此不复用 `judge_schema.go` 的结构化 schema，走独立的极简数字解析（`extractLeadingFloat` 容错模型输出的多余文字/越界负数）。
3. 判分用 Provider 复用 `ChatHandler.Registry.PickProvider("default")` → `("general")` 兜底链，与 `system_prompt.go` 组装系统提示词的取用方式完全一致，不为打分单独引入新的 Provider 解析路径或新的配置项。
4. 覆盖全部 4 条生产 assistant 回复落点（交互式 SSE 主路径 `sse.go` / 频道 webhook `webhook_receive.go` / cron 定时任务 `cron_runner.go` / workflow 步骤 `workflow_engine.go`），而非只接主路径——四条路径共享同一 `ChatHandler.SampleAndScoreReply` 方法，逐一接线成本低，且退化检测样本若只覆盖交互式流量会漏检自动化任务侧的模型质量劣化。
5. `onDegradation` 回调保持 `nil`：M9 autoRollback 触发链依赖 `dummyImmuneGateway` 之外的真实免疫网关实现（`cmd/polaris/boot_immune.go` 当前是"Empty implementation per requirements"占位），是独立待设计项，本次不强行接伪回调造成"看起来已闭环"的假象。

## 后果

- **正向**: `ContinuousSamplingMonitor` 从"读侧完整、写侧空转"变为真正闭环运行；四条生产路径的模型回复质量被统一纳入退化检测窗口。
- **负向**: 新增持续 LLM 调用成本（约 1% 生产回复流量）与对应延迟（异步，不阻塞主响应路径，但占用后台并发/Provider 配额）；用户对话内容会被发送给额外的评判模型调用，构成新增的数据流出面，需与现有隐私/数据处理策略保持一致。
- **反例守护**: 未来如有人提议"退化检测样本不够密集，把抽样率从 1% 调高"，需重新评估 LLM 调用成本与本 ADR 记录的隐私面，而非直接改常量；如有人提议"用规则替代 LLM Judge 省成本"，需注意这会偏离 M12 §9"共享 LLM Judge 引擎"的既定设计语义，退化检测精度会下降，应视为新的独立决策而非本 ADR 的自然延伸。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 仅接入主交互路径（sse.go），跳过 webhook/cron/workflow 三条次要路径 | 用户明确选择"按文档全量实现"；且退化检测若遗漏自动化任务流量，会对该类场景的模型质量劣化完全失明 |
| 用免费启发式（响应非空/无错误标记等）替代 LLM Judge 打分 | 偏离 M12 §9"共享 LLM Judge 引擎"的既定设计语义，退化检测精度显著下降；用户在四个选项中明确未选择此项 |
| 暂不接入，记录为待设计项（对齐 `dummyImmuneGateway` 的处理方式） | 用户明确选择立即按文档全量实现，而非推迟 |

## 引用代码

- `internal/eval/analysis/sampling_scorer.go`（`MaybeSampleAndScore`/`judgeReplyQuality`/`extractLeadingFloat`）
- `internal/gateway/server/chat/sessions_helpers.go`（`ChatHandler.SampleAndScoreReply`）
- `internal/gateway/server/chat/sse.go`、`internal/gateway/server/sysadmin/channelsadmin/webhook_receive.go`、`internal/gateway/server/sysadmin/cronadmin/cron_runner.go`、`internal/gateway/server/sysadmin/workflowadmin/workflow_engine.go`（4 条接入点）
- `internal/gateway/server/server_setters_sampling.go`（`Server.SetSamplingMonitor`）
- `cmd/polaris/boot_agent.go`（`AgentBundle.SamplingMonitor` 构造）、`cmd/polaris/boot_server.go`（注入）
- `docs/arch/M12-Eval-Harness.md §9`（设计文档，已同步订正"已实现"断言并补充本次接入点说明）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-14 | 初稿，记录写侧生产接入决策与用户确认的成本/隐私权衡 |
