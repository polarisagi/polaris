# 06 代码审查清单

> 用于 commit 前、PR 前的逐项自查。

## C1 架构合规性

- [ ] 包依赖方向符合 04-Module-Boundary.md B1
- [ ] 新接口在 `internal/protocol/` 定义（而非实现方），带 `@consumer/@producer/@arch` 注解
- [ ] 跨模块事件是结构化类型（而非字符串 topic）
- [ ] Rust 变更同步更新了 `internal/protocol/ffi-abi.md`

## C2 代码结构

- [ ] 文件结构顺序：pkg → import → const → type → struct → New → methods → helpers
- [ ] `doc.go` 是唯一的包级注释文件
- [ ] 构造函数 `NewXxx` 显式接收所有依赖（无 `init()`）
- [ ] 如果 `InjectLLMProvider` 模式：明确注释说明这是 Tier1+ 可选注入，Tier0 可 nil

## C3 错误处理

- [ ] 所有 error 返回使用 `perrors.Wrap(CodeXxx, msg, err)`
- [ ] 无裸 `error`、`fmt.Errorf`、`errors.New` 在 `pkg/` 和 `internal/` 中
- [ ] 降级路径有日志记录（Warn level），非静默失败

## C4 注释质量

- [ ] comment 只写"为什么"，不写"是什么"
- [ ] 中文
- [ ] 无冗余注释（如 `// NewXxx 创建 Xxx 实例`——命名已经表达了）

## C5 测试覆盖

- [ ] 测试文件与被测文件同级同包
- [ ] 表驱动测试（参考已有测试风格）
- [ ] 覆盖正常路径 + 边界值 + 错误路径
- [ ] `make test` 通过
- [ ] 对 AI 生成的测试：确认没有只覆盖 AI 自己知道的路径（加 reviewer 的边界测试）

## C6 提交质量

- [ ] 提交信息格式：`<type>(<scope>): <简述>` / scope = 包名
- [ ] 提交粒度：一次提交解决一个逻辑变更
- [ ] 不含测试文件的提交必须有理由
- [ ] 不含对未损坏代码的顺手修改（100% 指令溯源性）

## C7 批量评审指引（AI 生成变更摘要）

提交前，AI 必须输出变更摘要供人类跳审：

```markdown
## 变更摘要
- 总文件数: N（新增 M，修改 K，删除 0）
- 核心逻辑变更: 2 句话说明
- 高风险区域: 文件名 + 风险描述
- 建议 reviewer 重点关注: 文件名 + 关注理由
- 测试覆盖率: 新代码覆盖 __%
```

## C8 参考实现对齐

- [ ] PR 描述已 link `pkg/xxx/yyy.go` canonical 文件
- [ ] 结构（文件顺序、构造函数风格、helper 位置）与 canonical 一致
- [ ] 命名（标识符词根、错误码、指标名）与 canonical 及 `docs/arch/00-Global-Dictionary.md §13` 一致
- [ ] 偏离已写明原因，且偏离不构成"事实新 canonical"

## C9 PR 体积

- [ ] diff 满足 `00-Constitution.md R8` 上限；超出已 link ADR 或拆分计划
- [ ] 1 PR = 1 逻辑变更，无夹带顺手修改（R1 指令溯源性）
- [ ] 契约 / 破坏性变更走 `05-Coding-Workflow.md W6.2` + `04-Module-Boundary.md B5`
- [ ] R7 lint 通过：funlen / gocyclo / nestif
