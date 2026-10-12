# ADR-0112: AI 协作规范 2026-10 修订——死规则清理、多代理契约、交付证据与冲突裁决

> 状态：Accepted ｜ 首次：2026-10-12 ｜ 最近复核：2026-10-12
> 范围：根 `CLAUDE.md`、`docs/specs/00/05/06`、`docs/arch/00-Global-Dictionary.md`、ADR-0087、M09、M13；`internal/lint/inv_lint_test.go`、`internal/learning/optimizer/rollout.go`、`tools/docs_gen.go`。
> 原则：规则要么有门控执行，要么是可审查的明确条款；两者都不是的删除。为旧模型短板写的补偿性约束删除；新模型与多代理协作暴露的失效模式补规则。HE-1~7 与五防线是架构约束，与模型能力无关，不在本次放宽范围。

## 依据（2026-09-15 ~ 2026-10-12，244 次提交实测）

| 规则 | 实测 |
|---|---|
| R8 / C9「单 PR diff ≤ 300 行」 | 近期功能提交非测试非文档 diff：3864 / 1253 / 985 / 706 行；标 `[oversized]` 仅 2 次 |
| W6.2「契约 `[spec]` / 实现 `[impl]` 分提交」 | 自 2026-08-01 起仅 4 次 |
| W2「阶段 A 人工审核，批准前不写代码」「响应开头声明已读」 | 实际流程为 Opus 设计 → Sonnet 实现，无人工审核环节；声明不被任何机制校验 |
| R7「函数体 ≤ 60 行」 | `funlen` 刻意不启用（ADR-0091），无任何门控 |
| W5「超 20 轮注意力衰减」 | 旧模型经验；当前真实问题是长任务中断后的续接（2026-10-12 文档轮靠 `PLAN.md` 续接成功） |

## 决策一：删除或改写失效规则

- **R8 / C9**：删除「≤ 300 行」。改为「1 提交 = 1 逻辑变更；非测试 diff > 1000 行的提交，提交信息须引用 ADR 或计划文件」（审查项，无门控）。`[oversized]` 标记废止。
- **W6.2**：删除 `[spec]` / `[impl]` 分提交要求；破坏性契约变更仍走 `04 B5`。
- **W2**：删除阶段 0 声明格式与「Human 拒绝进入阶段 A」「批准前不写实现」。改为：跨模块 / 契约 / Schema 变更先产出设计（ADR 或计划文件）再实现；单模块内变更直接实现。测试要求保留为「测试与实现同提交，覆盖正常 / 边界 / 失败路径」，不再强制逐文件先测后写的顺序。
- **R7**：函数体 ≤ 60 行降为审查项并标「无门控」；其余有门控的维度不变。删除「AI 生成长函数无内省压力」类理由句（R7、R8）。
- **W1 / CLAUDE.md**：删除「启动预算 ≈ 12K token，不可再增」硬数字；按需加载原则保留。
- **W5**：删除「超 20 轮注意力衰减 / 一任务一会话」；由决策三的计划文件规则取代。
- **交互纪律**：「禁止问候 / 拟人 / 情感确认 / 修饰词」等压为一句「结论前置，无寒暄与铺垫」。中文输出、100% 指令溯源、歧义主动提问保留。

## 决策二：交付必须附证据

- 声明「完成 / 通过 / 全绿」须附实际执行的命令与关键输出（`make lint`、`go test`、对应门控）；未运行的检查必须写明「未运行」及原因。
- 验收他人（含其他模型、子代理）交付时先自行复跑，不采信自述。
- 落点：`05` 阶段 C、`06` 新增 C10。

## 决策三：多代理协作契约（新增 `05 W8`）

- **分工**：设计方（架构 / 方案）与实现方（编码 / 文档修改）可为不同模型；设计方产出的计划文件即契约，实现方不得改动契约范围外的文件。
- **计划文件**：多步骤或跨会话任务在 `local_playground/<任务>/PLAN.md` 落盘，含目标、硬约束、文件分配、`§进度` 节；中断后从 `§进度` 续接。
- **文件所有权**：并行执行时每个文件只分配给一个执行者；会改写共享文件的工具（`make docs-sync`、`make docs-gen`、`make gen-threshold-examples`、`make generate-manifest`）只由收尾者在并行结束后统一运行。
- **汇报是待核实的陈述**：子代理汇报中的事实结论须由设计方抽查或复跑门控后才算成立。
- **冲突只记录不改**：执行中发现规范与代码冲突，记入汇报交设计方裁决，不得顺手改规范或改代码迁就。

## 决策四：不可逆操作须确认

`git push`、删除开发库 / 数据目录、发布签名与密钥操作、删除已跟踪文件（计划内的合并删除除外）、改写 git 历史——执行前须经用户确认。Git 署名规则不变。

## 决策五：审核结论回代码取证（`06` 新增 C11）

外部模型审核报告的每条发现须回代码取证后才可修复；「未接线」「无调用方」类结论必须 `git grep` 调用点与装配点。依据：Gemini 全局审核误报率约 23%，其中「未接线」类几乎全部误报。CI 对抗审查（`scripts/constitutional_review.sh`）维持 warning-only，`REVIEWER_MODEL` 须与开发模型不同族。

## 决策六：规范缺口补齐

- `00 R3` 补 `R3-HE7 防退化边界`：自查项为「是否绕过 ExecuteTool 写记忆、是否弱化 Taint / Cedar / KillSwitch / SSRFGuard / 能力令牌任一防线、安全门是否 fail-closed」。
- `Test_inv_NoCrossLayerImport`：`execute` 纳入 L1、`runtimeinfo` 纳入 L0（`lint` 为测试包不入层）。若纳入后出现存量违例，按 R1.7 处置并在本 ADR 沿革记录，不得为通过而缩小规则。
- `state.yaml` 漂移门控（08 H3）：维持「尚无门控」如实标注，列为后续工作。

## 决策七：2026-10-12 文档轮遗留冲突裁决

| # | 冲突 | 裁决 |
|---|---|---|
| 1 | XR-12 / ADR-0087 决策五依赖 `internal/bootstrap`（全仓零 import），实际装配在 `cmd/polaris/boot_*.go` | 手工装配为规范。ADR-0087 决策五改为「长驻任务继承进程根 ctx（`cmd/polaris` 创建并经 boot 装配注入）」；XR-12 去掉对 bootstrap 的依赖表述。`internal/bootstrap` 的去留按 ADR-0088 重新评估触发条件（装配链 > 6000 行，现 5805）处理，本 ADR 不删除 |
| 2 | 03 AGENT-7 Tier-0 CodeAct | 维持已改：容器请求降级 SandboxNativeOS，仅 `CapPrivileged` 拒绝（ADR-0008 决策一） |
| 3 | 灰度阶梯：`state.yaml` `canary_steps: [0.01, 0.10, 0.50, 1.00]` vs `rollout.go` 硬编码 1/5/25/50/100 | `state.yaml` 是规范性 SSoT，改代码为 1/10/50/100 |
| 4 | M04 §4.3 补偿声明 | 无冲突：M04 已按 ADR-0088 2026-09-20 复核改为不强制（误报） |
| 5 | M09 §6「GraphRAG LLM 调用日预算超限」与 M10 inv_M10_05、代码（无日预算） | 删除 M09 该行 |
| 6 | M13 §8.9 禁止 `translateX` 手工侧滑 vs 日志抽屉实现 | 无冲突：日志抽屉已是 DaisyUI `drawer`（`web/src/index.html`，误报）。另发现 `web/src/pages/automation.html` 运行记录面板用 `translate-x-*` 手工侧滑，属 §8.9 违例，列为前端后续工作 |
| 7 | `tools/docs_gen.go` M04 摘要「状态机 13 态」 | 改为 14 态并重新生成 INDEX 生成块 |
| 8 | 杂散文件 `internal/knowledge/patch_chunk.py`、`internal/knowledge/graphrag/coverage.out` | 删除 |

## 重新评估触发条件

- 出现因 diff 过大导致的漏审缺陷 → 重评决策一 R8（考虑 CI 机械门控）。
- 子代理交付在设计方复核中连续出现事实性错误 → 收紧决策三（如强制门控复跑后才可汇报）。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-10-12 | 首次。依据 2026-10-12 文档第四轮精简的复核结论。落地时发现 `config.M9SelfImprove.CanarySteps` 早已存在但无读取方，`NewProgressiveRollout` 改为读取该配置；`ConfirmShadow` 的硬编码 5% 同步改为阶梯第二档 |

## 引用代码

`internal/lint/inv_lint_test.go`、`internal/learning/optimizer/rollout.go`、`tools/docs_gen.go`、`scripts/constitutional_review.sh`、`docs/arch/spec/state.yaml`
