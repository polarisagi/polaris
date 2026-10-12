# 06 代码审查清单

> commit 前、PR 前逐项自查。

## C1 架构合规性

- [ ] 包依赖方向符合 `04 B1`
- [ ] 新接口在 `internal/protocol/` 定义（非实现方），带 `@consumer/@producer/@arch` 注解
- [ ] 跨模块事件是结构化类型（非字符串 topic）
- [ ] Rust 变更同步更新 `internal/protocol/ffi-abi.md`
- [ ] 安全门依赖 fail-closed、nil 不旁路（R1.14）；MCP stdio 子进程经 `sanitizeParentEnv()`（R1.15）

## C2 代码结构

- [ ] 文件结构顺序符合 `01 F1`；`doc.go` 是唯一包级注释文件
- [ ] `NewXxx` 显式接收所有依赖，无 `init()`
- [ ] `InjectLLMProvider` 模式：注释明确"Tier1+ 可选注入，Tier0 可 nil"

## C3 错误处理

- [ ] 所有 error 返回用 `apperr.Wrap(apperr.CodeXxx, msg, err)`；`internal/` 业务代码无裸 `error`、`fmt.Errorf`、`errors.New`
- [ ] 降级路径有 Warn 日志，非静默失败

## C4 注释质量

- [ ] 中文，只写"为什么"，不写"是什么"（如 `// NewXxx 创建 Xxx 实例` 属冗余）

## C5 测试覆盖

- [ ] 测试与被测文件同级同包；表驱动；覆盖正常 + 边界 + 错误路径；`make test` 通过
- [ ] AI 生成的测试：确认没有只覆盖 AI 自己知道的路径（补 reviewer 边界测试）
- [ ] 涉及 Schema 变更：已按 `05 W7`（直改建表文件，禁 ALTER TABLE 补丁）

## C6 提交质量

- [ ] 格式 `<type>(<scope>): <简述>`，scope = 包名；一次提交一个逻辑变更
- [ ] 不含测试文件的提交有理由；不含对未损坏代码的顺手修改
- [ ] 门控命令与 Git 署名以根 `CLAUDE.md §编码约定` 的 `[强制]` 条目为准——此处刻意只放指针，不复制清单（避免多出一处人工同步面）

## C7 变更摘要

提交前 AI 输出变更摘要供人类跳审，格式同 `05 W6.3` 的「变更摘要」段。

## C8 参考实现对齐

- [ ] PR 已 link `internal/xxx/yyy.go` canonical 文件
- [ ] 结构（文件顺序、构造函数风格、helper 位置）与 canonical 一致
- [ ] 命名（词根、错误码、指标名）与 canonical 及 `docs/arch/00-Global-Dictionary.md §13` 一致
- [ ] 偏离已写明原因，且不构成"事实新 canonical"

## C9 PR 体积

- [ ] 非测试 diff > 1000 行的提交已引用 ADR 或计划文件（`00 R8`，无门控）
- [ ] 1 PR = 1 逻辑变更，无夹带修改
- [ ] 破坏性契约变更走 `04 B5`（`05 W6.2` 已不要求分提交）
- [ ] R7 lint 通过（gocyclo / nestif；`funlen` 不启用，ADR-0091）

## C10 交付证据

- [ ] 「完成 / 通过 / 全绿」声明附实际执行的命令与关键输出；未运行的检查写明「未运行」及原因
- [ ] 验收他人（含其他模型、子代理）交付时已自行复跑，未采信自述（`05 W8`，ADR-0112）

## C11 审核结论取证

- [ ] 外部模型审核报告的每条发现已回代码取证后才修复（ADR-0112）
- [ ] 「未接线」「无调用方」类结论已 `git grep` 调用点与装配点（Gemini 全局审核误报率约 23%，此类几乎全为误报）
- [ ] CI 对抗审查（`scripts/constitutional_review.sh`）维持 warning-only，`REVIEWER_MODEL` 与开发模型不同族
