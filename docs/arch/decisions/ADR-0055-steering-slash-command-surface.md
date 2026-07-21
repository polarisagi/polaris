# ADR-0055: `/steer` 激活引导命令面接线

- **状态**: Accepted（已执行）
- **日期**: 2026-07-21
- **决策者**: MrLaoLiAI
- **相关模块**: `internal/llm/adapter/`、`internal/gateway/server/chat/`、
  `internal/gateway/server/`、`cmd/polaris/boot_substrate.go`、`boot_server.go`

## 上下文

承接 ADR-0052 DEFER 条目：`adapter.SteeringAdapter.SteerActivations`/
`.ClearSteering` 适配器骨架 + Tier 门控已完整（`boot_substrate.go` 构造），
但 `/steer` 用户命令解析面全仓库不存在。`M09-Self-Improvement-Engine.md`
§1.3 已定义命令面："`/steer list|set <label> <weight>|deactivate|delete|
calibrate-layer <task_type>`"。用户此前对本任务的决策："我先给出合理默认值
草案再实现，你后续校准"。

读代码后发现 `internal/gateway/server/chat/slash_commands.go` 已有完整的
斜线命令基础设施（`SlashCommandRouter.Dispatch`，服务 `/context`/`/compact`/
`/clear`/`/help`，真实接线于 `sse.go:170`），是现成的、经过验证的接入点，
不需要新建命令解析层。

## 决策

### 实现范围

`/steer list|import <label> <file>|set <label> <weight>|deactivate|
delete <label>`：

- `internal/llm/adapter/control_vector_store.go`（新增）：`ControlVectorStore`
  线程安全 label→`ControlVector{Vector, Layer}` 注册表，进程内存储。
- `internal/gateway/server/chat/slash_command_steer.go`（新增）：
  `handleSteer` + 子命令处理器，接入 `SlashCommandRouter.Dispatch` 的
  `/steer` case。
- `SlashCommandRouter.SetSteering(steering, cvStore)`：新增可选注入方法
  （二者均可为 nil，nil-safe 降级为提示信息）。
- `Server.SetSteering`（`server_setters_steering.go`，与 `SetSamplingMonitor`
  同一拆分/注入风格）→ `cmd/polaris/boot_server.go` 调用
  `httpServer.SetSteering(sb.Steering, sb.CVStore)`。
- `SubstrateBundle.CVStore`（`boot_substrate.go`）：`FeatureActivationSteer`
  门控开启时与 `steeringAdapter` 同步构造。

### 范围内的默认值决策（用户已授权"提议默认值"）

- 控制向量默认注入层 `layer=15`：直接采用 `M09-Self-Improvement-Engine.md`
  §1.3 文档已明确写出的默认值（"默认 layer_id=15"），非我方臆测。
- `/steer import` 的向量来源：M09 §1.3 只定义了管理已存在 CV 的命令
  （list/set/deactivate/delete/calibrate-layer），未定义 CV 如何产生——
  全仓库确认没有任何提取/训练 Control Vector 的流水线（区别于 QLoRA/PRM，
  参见 ADR-0056，那是完全不同的技术路线，产出模型权重/奖励模型而非
  hidden_state 偏移向量）。新增 `import <label> <file>` 子命令读取外部
  JSON（`{"layer":15,"vector":[...]}`），作为"提议的默认接入方式"——这是
  本任务范围内唯一需要我方新增设计决策的点，理由：没有这一步整条命令面
  在没有任何真实 CV 数据时完全无法验证，属于"合理默认值"而非臆测业务语义
  （HE-3 意义上，import 只是把已有数据搬进内存，不发明"如何生成 CV"这一
  真正的算法问题）。

### 不在本次范围（诚实标注，不臆测实现）

- **`calibrate-layer <task_type>`**：M09 §1.3 命令面文档写了这一子命令，
  但"校准"语义要求对同一 task_type 在多个 `layer_id` 下运行效果评估后择优
  ——现有 Eval Harness（`internal/eval/harness/`）没有对应的 case 类型，
  实现前需要先设计"按 layer_id 分组的评估轮次"这一新机制。命令返回明确的
  "暂未实现"提示（非静默失败/非伪造结果）。
- **"成功率 <0.1 自动停用"**（§1.3 正文，非命令面本身）：需要一个"本次
  steering 是否成功"的反馈信号——全仓库当前没有把 LLM 回复质量/用户反馈
  关联回具体 Control Vector 应用实例的机制（这本身是一条独立的"结果归因"
  设计问题，类似 M9 Reflexion 的按轨迹归因，但对象换成 Control Vector）。
  本次不臆测"成功"定义，不实现。
- **持久化跨重启**：`ControlVectorStore` 是进程内存储，重启后需要重新
  `import`。是否需要 DB 持久化取决于产品侧运营方式决策，非本次桥接范围。

## 判断依据

延续 R1：`SteerActivations`/`ClearSteering` 的桥接缺口（命令解析面）是
真实且可定位的——`SlashCommandRouter` 已有现成、已验证的接入模式，只需照
搬 `/context`/`/compact` 的写法新增一个 case。`calibrate-layer` 与"成功率
自动停用"则不满足 WIRE 标准——二者依赖的支撑机制（分层评估轮次、结果归因
信号）在仓库中均不存在，是需要独立设计的新功能，不是"一次桥接"。

## 后果

- **正向**：`go build`/`go test ./...`/`golangci-lint` 全绿；`deadcode`
  确认 `SteeringAdapter.SteerActivations`/`.ClearSteering` 不再出现；新增
  `internal/llm/adapter/control_vector_store_test.go`、
  `internal/gateway/server/chat/slash_command_steer_test.go` 覆盖
  import/list/set/deactivate/delete/calibrate-layer-not-supported 全路径。
- **负向**：`calibrate-layer` 与成功率自动停用仍未实现；`ControlVectorStore`
  无持久化，重启后需重新 import。

## 引用代码

- `internal/llm/adapter/control_vector_store.go`（新增）
- `internal/llm/adapter/steering.go`（原有，本次未修改，`SteerActivations`/`ClearSteering` 现被真实调用）
- `internal/gateway/server/chat/slash_command_steer.go`（新增）
- `internal/gateway/server/chat/slash_commands.go`（`/steer` case + 字段/Setter）
- `internal/gateway/server/server_setters_steering.go`（新增）
- `cmd/polaris/boot_substrate.go`（`CVStore` 构造）、`boot_server.go`（`SetSteering` 调用）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-21 | 初稿：`/steer list/import/set/deactivate/delete` 接线完成；`calibrate-layer`/成功率自动停用标注为独立未实现项 |
