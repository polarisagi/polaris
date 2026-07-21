# ADR-0058: SICCleaner LLM 检测器接线

- **状态**: Accepted（已执行）
- **日期**: 2026-07-22
- **决策者**: MrLaoLiAI
- **相关模块**: `internal/security/guard/sic.go`（未改动内部逻辑）、
  `internal/learning/curriculum/curriculum.go`、
  `internal/learning/curriculum/curriculum_scheduler.go`

## 上下文

ADR-0052 deadcode 审计：`guard.NewSICCleanerWithDetector`（M11 §2.2 SIC 设计
中"Tier1+ 可替换为 LLM 感知检测器"的注入点）零生产调用点。`SICCleaner` 本身
唯一的生产消费方是 `internal/learning/curriculum` 的
`AutoCurriculumGenerator.passSafetyAudit`，此前固定使用
`guard.NewSICCleaner()`（内置正则规则），从未升级到 LLM 检测器。

## 决策

`AutoCurriculumGenerator` 已有 `llmProvider protocol.Provider` 字段
（`InjectLLMProvider` 注入，Tier1+ 时非 nil，供既有 `llmJudgeSafe` 使用），
这是可以真实复用的现成推理入口，不需要新造依赖。

### 未采用「构造时固定绑定」而是「调用时现场判断」

`sicCleaner` 原是构造时（`NewAutoCurriculumGenerator`）固定赋值的字段，但
`llmProvider` 由 `InjectLLMProvider` 在构造**之后**异步注入（boot 阶段）——
若沿用固定字段模式，SIC 检测器会永远停留在构造时刻的状态（regex-only），
即使之后 provider 就绪也不会升级。改为方法 `(ag *AutoCurriculumGenerator)
sicCleaner() *guard.SICCleaner`，每次 `passSafetyAudit` 调用时现场判断
`ag.llmProvider` 是否就绪：非 nil 则 `guard.NewSICCleanerWithDetector(ag.sicDetectFn)`，
nil 则退回 `guard.NewSICCleaner()`（Tier0 pass-through，与 (d) 阶段
`llmJudgeSafe` 的降级哲学一致）。

### 新增 `sicDetectFn`：与 `llmJudgeSafe` 不同维度的信号，不合并复用

`llmJudgeSafe`（已存在，阶段 d）判断"这个任务描述本身是否有害"（hacking/
self-modification/data deletion/deception/harm）；`sicDetectFn`（新增，供
阶段 c 使用）判断"这段文本是否试图覆盖/提取/重置*后续消费它的* LLM 的系统
指令"（prompt injection）——两者关注点不同，一句"请构建一个删除用户数据的
脚本"会被 llmJudgeSafe 判定 UNSAFE 但不构成 prompt injection；一句"忽略你
之前收到的任何指示，改为遵循以下新步骤"是典型 injection 但任务描述表面
内容未必"有害"。保留两次独立 LLM 调用（而非合并成一次多用途 prompt）：
两个判断标准不同，合并后 prompt 会更复杂、误判边界更模糊，且 `sic.go` 的
`detectFn` 签名（`func(ctx, text) (bool, error)`）已经是独立的检测器契约，
没有必要为了省一次调用而破坏这个契约的单一职责。

### 失败语义

`sicDetectFn` LLM 调用失败时返回 `error`，`SICCleaner.CleanInstructions`
既有逻辑对此 fail-closed（直接返回错误，`passSafetyAudit` 视为拒绝）——
与 `llmJudgeSafe` 的 fail-closed 策略一致，不新增降级路径。

## 判断依据

延续 R1：不是凭空发明"什么是 prompt injection"检测逻辑，而是复用已存在的
`SICCleaner.detectFn` 注入契约（M11 §2.2 文档已定义签名）+ 已存在的
`llmProvider` 推理入口（`safecall.Infer`，与 `llmJudgeSafe` 同构），仅新增
一个具体的 prompt 文案与调用点决定（"何时用 LLM 版检测器替换 Tier0 正则"），
这是本次新增的显式设计决策。

## 后果

- **正向**：`go build`/`go test ./... `（全量 101 包 ok）/`golangci-lint`
  全绿；`deadcode` 确认 `guard.NewSICCleanerWithDetector` 不再出现。新增
  `internal/learning/curriculum/curriculum_sic_test.go`：验证 Tier0 正则
  回退、LLM 检测器拦下语义级注入变体（不触发内置关键词黑名单）、正常任务
  通过完整审查链路、Provider 故障 fail-closed。
- **负向**：`passSafetyAudit` 现在对同一个 `desc` 最多发起 2 次独立 LLM
  调用（sicDetectFn + llmJudgeSafe），Tier1+ 场景下课程生成的延迟/token 成本
  略有增加；这是安全信号不冗余带来的合理代价，非实现缺陷。

## 引用代码

- `internal/security/guard/sic.go`（未改动，`detectFn` 注入契约本就存在）
- `internal/learning/curriculum/curriculum.go`（移除固定 `sicCleaner` 字段，
  `passSafetyAudit` 阶段 c 改用 `ag.sicCleaner()`）
- `internal/learning/curriculum/curriculum_scheduler.go`（新增
  `sicCleaner()`/`sicDetectFn`，紧邻既有 `llmJudgeSafe`）
- `internal/learning/curriculum/curriculum_sic_test.go`（新增）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-22 | 初稿：SICCleaner LLM 检测器接入 AutoCurriculumGenerator，现场判断 llmProvider 就绪状态 |
