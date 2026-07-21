# ADR-0060：M4 ContextWindowManager 热路径压缩接入 + M4/M5 共享压缩算法抽取

## 状态

已接受，已实现。

## 背景

deadcode 审计任务 #18（ADR-0052 DEFER 项）：`agent.ContextWindowManager.
NeedsCompaction` 从未被构造、`currentUsage` 从未被赋值。`M04-Agent-Kernel.md
§7` 明确要求："M4 ContextWindowManager 持有热路径阈值（>70% salience 排序候选
/>90% 语义结构感知逐出），M5 SessionCompressor 在 M4 调用时执行实际的冷压缩
算法"，但代码里两者从未连接。

ADR-0052 当时的结论是"需要决定 M4 Agent kernel 主循环如何跨层调用 M5
SessionCompressor（后者操作 chat_messages 表行，前者操作 agent 内核内部 token
计数），是架构设计问题非一行接线"。

## 核实与澄清

深入代码后发现表述有误导性：M5 `SessionCompressor`（`internal/gateway/server/
chat.Compressor`）的 `Compact/ForceCompact` 操作的 `[]apptypes.Message`，与 M4
Agent 内核每次 LLM 调用组装的 `reqMsgs []types.Message`，其实是**同一个 Go
类型**（`pkg/types.Message`，只是导入别名不同）。真正的障碍不是"消息表示不
同"，而是原实现把 Stage1(tool_result 卸载)/Stage2(LLM 锚点摘要)/Stage3(画布
注入) 这套纯算法，与网关专属关注点（chat_messages 持久化回写、hook 触发、
thrashing 计数、配置来源）耦合在同一个 `*chat.Compressor` 结构体里，M4 无法
单独复用算法而不引入对整个网关包的依赖。

另需区分：M4 主循环已有的 50/75/100% 三级检测（`agent_execute_effect.go`）
检测的是**任务级累计 token 预算**（`sCtx.TokensUsed/TokenBudget`），触发时只
做「注入 `[BUDGET_CONSTRAINT]` 提示收紧 DAG 规模」和「100% 硬熔断任务失败」；
`ContextWindowManager` 要防的是**单次 LLM 调用的 reqMsgs 实际大小**（S_EXECUTE
多轮工具调用可能在同一个任务预算内把单次请求的消息堆到超出 Provider 上下文
窗口）。二者是互补的两个维度，互不替代。

已通过 `AskUserQuestion` 向用户确认处理方向：从三个选项（提炼共享算法 / 仅
在 S_EXECUTE 内做 tool_result 卸载 / 只订正文档不开发）中选择"提炼共享压缩
算法"。

## 决策

1. 新建 `internal/memory/compact`（L1，与 M05 spec"SessionCompressor 实现见
   internal/memory/"的表述首次对齐），抽出 Stage1/2/3 算法本体为纯函数（`
   RoughTokens`/`SplitMessages`/`CalcSummaryBudget`/`BuildTranscript`/
   `Summarize`/`InjectTaskCanvas`/`OffloadLargeToolResults`），只依赖
   `protocol.Provider` 与窄接口 `compact.Offloader`（HE-3），不依赖任何持久化
   或网关专属状态。
2. `internal/gateway/server/chat.Compressor` 重构为委托调用
   `internal/memory/compact` 的对应函数，只保留网关专属部分（`persistCompacted`
   持久化回写、hook 触发、thrashing 防抖计数、配置阈值来源）。算法测试随迁移
   至 `internal/memory/compact/compact_test.go`。
3. `agent.ContextWindowManager` 补齐 `NewContextWindowManager(maxTokens)` 构造
   函数（默认 90000，M04 §7）与 `SetCurrentUsage`/`MaxTokens` 方法；`Agent` 新增
   `cwm *ContextWindowManager`（`NewAgent` 默认构造）与 `toolOffloader
   compact.Offloader`（可选注入，nil 时 Stage 1 静默跳过，与网关侧同语义）
   两个字段。
4. 新增 `agent_context_compaction.go`：`hotPathCompactIfNeeded` 在
   `agent_execute_effect.go` 每次组装完 `reqMsgs`、PII tokenize 之前调用——
   更新 `currentUsage`，触发时执行 `hotPathCompact`：
   - 软触发（>70%）：只做 Stage 1（tool_result 卸载），不调用 Provider——
     M04 §7 提到的"salience 排序"目前在代码库中没有可复用的、针对任意
     `reqMsgs` 条目的显著性打分实现（既有 salience 概念全部作用于持久化
     episodic_events 行，粒度不同），发明一套新的打分算法没有 spec 依据，
     属于 R1 禁止的臆测；用"大 tool_result 优先卸载"作为诚实、保守、已有
     实现支撑的替代，并在此处明确记录这一简化，不假装做了更复杂的事。
   - 硬触发（>90%）：在 Stage 1 基础上追加 Stage 2（LLM 摘要）+ Stage 3
     （`a.memory.RenderTaskCanvas()` 画布注入，Agent 本就持有该接口，无需
     新增插件）。只在真正逼近上限时才动用有真实推理成本的 LLM 摘要调用，
     避免每次越过 70% 线就多花一次 Provider 调用。
   - ReplayMode 下物理短路（与其余 4 处 `IsReplaying()` 短路点同一语义）。
5. `cmd/polaris/boot_agent.go`：`a.InjectToolRefOffloader(tb.ToolRefOffloader)`
   ——与网关 Compressor（`boot_server.go SetToolRefOffloader`）共用同一个
   `internal/memory.ToolRefOffloader` 实例，两条压缩路径卸载的内容落在同一份
   `workspace_vfs` 索引下，`read_tool_ref` 均可回读。

## 顺带修正

- `Compressor.summarize`（重构前）构造了 `apptypes.InferRequest{MaxTokens,
  Temperature}` 却从未把这两个字段传给 `safecall.StreamInfer`（只传了
  `.Messages`，没有 opts）——`compact.Summarize` 改为显式传入
  `types.WithMaxTokens(maxTokens)`/`types.WithTemperature(0.3)`，是重构中
  顺带修复的一个真实但此前无害（摘要长度只是没被预算约束，不算安全问题）的
  静默失效字段。
- `compressor_helpers.go` 中一段紧邻 `offloadLargeToolResults` 的注释仍写着
  "工具输出预裁剪暂未在 chat 压缩路径实现"，但该函数当时已完整实现并真实
  接入 `ToolRefOffloader`——典型的注释漂移（见 memory 记录
  `polaris-comment-drift-bug`），随本次改动一并订正。

## 验证

- `go build ./...`、`go vet ./...`（仅剩预置 FFI `unsafe.Pointer` 警告）
- `make lint`：主 lint + wasip1 子 lint 均 `0 issues`（含一次 `internal/agent/
  agent_wiring.go` 414→398 行的 R7 文件行数治理，新增注入方法改放
  `agent_context_compaction.go` 而非计入已接近上限的 wiring 文件）
- `go test ./...`：全量通过
- `go test -race ./internal/agent/... ./internal/memory/... ./internal/gateway/
  server/chat/... ./cmd/polaris/...`：通过
- `deadcode ./cmd/polaris/...`：`ContextWindowManager.NeedsCompaction` 不再被
  标记（确认已接入调用链）；`TaintedJSONNode.AllStrings`/`BuildIdempotencyKey`
  仍按预期标记（分别是任务 #16/#14 的既定 DEFER 结论，非本次遗漏）
- 新增测试：`internal/memory/compact/compact_test.go`（算法本体，随迁移
  一并保留原断言）、`internal/agent/agent_context_compaction_test.go`
  （`hotPathCompactIfNeeded` 低于阈值不动/软触发只卸载不调用 Provider/硬触发
  调用 Provider 并显著缩减消息数/ReplayMode 物理短路 四个场景）

## 影响范围

新增 `internal/memory/compact`；重构 `internal/gateway/server/chat/
compressor.go`+`compressor_helpers.go`（委托而非重写行为）；`internal/agent/
budget.go`+`agent.go`+`agent_wiring.go`+新增 `agent_context_compaction.go`；
`internal/agent/agent_execute_effect.go`（一处调用点插入）；`cmd/polaris/
boot_agent.go`（一行注入）。均为在既有测试覆盖下的委托重构 + 新增独立压缩
路径，未改变任何既有对外行为（网关 `/compact` 命令与自动压缩行为完全不变）。
