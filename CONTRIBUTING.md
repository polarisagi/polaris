# 贡献指南 / Contributing

欢迎贡献。本项目对 PR 质量与架构一致性有较高要求，请先读完本文。

## 1. 环境

```bash
git clone https://github.com/polarisagi/polaris.git
cd polaris
make build            # Rust FFI → Web UI → Go binary（≈ 2 min 首次）
make test             # go test ./pkg/... ./internal/...
make lint             # golangci-lint run ./...
make rust-test
make docs-check       # §跳读 行号一致性
make docs-lint        # 文档级 Go 代码块禁令 (#9)
```

依赖：Go 1.26+ / Rust 1.94+ / Node 20+（仅 Web UI）。

## 2. Spec-First 工作流（**强制**）

代码改动**前**必须按场景加载规范文档：

| 改动域 | 必读 |
|--------|------|
| 任何 PR | `docs/specs/INDEX.md` + `00-Constitution.md`（反模式 R1~R8）+ `05-Coding-Workflow.md` |
| Go 代码 | + `docs/specs/01-Go-Code.md` |
| Rust FFI | + `docs/specs/02-Rust-FFI.md` |
| Agent 行为 | + `docs/specs/03-Agent-Pattern.md` |
| 跨模块 | + `docs/specs/04-Module-Boundary.md` |
| 提交前 | + `docs/specs/06-Review.md` |

架构层：从 [`docs/arch/INDEX.md`](./docs/arch/INDEX.md) 入口按场景挑 1~3 个 `M_X.md`。**禁止全量加载** docs/arch/——会超 200K 上下文。

## 3. 改动前需 ADR 的场景

依赖选型 / 跨层例外 / 性能权衡 / 安全协议变更 / 反复被问的"为什么不用 X"——开 PR 前先去 [`docs/arch/decisions/`](./docs/arch/decisions/) grep 一下；现有 ADR 已驳回的方案不要重提。需要新增 ADR 时模板见 [`ADR-template.md`](./docs/arch/decisions/ADR-template.md)。

## 4. 提交信息

格式：`<type>(<scope>): <述>`（中文简述）

- `type` ∈ `feat` / `fix` / `refactor` / `docs` / `test` / `chore` / `perf`
- `scope` = 包名或模块（如 `cognition` / `M07` / `arch`）

例：`feat(cognition): 实现 ReflectionMemory 写入路径`

## 5. PR 检查清单

提交前确保以下通过：

- [ ] `make test` 全绿（含 `-race` 与 `spec_consistency_test`）
- [ ] `make lint` 无 warning
- [ ] `make docs-check` 通过（修了 markdown header 需 `make docs-sync`）
- [ ] `make docs-lint` 通过（M_X 中不引入 ```go 代码块 / 完整 func 签名）
- [ ] 跨模块接口变更同步 `internal/protocol/`
- [ ] 状态机变更同步 `docs/arch/spec/state.yaml`
- [ ] DDL 变更同步 `internal/protocol/schema/`
- [ ] 关键安全/选型决策附 ADR

## 6. 沟通

- 官网：[https://polarisagi.online/](https://polarisagi.online/)
- 作者全网同名 ID：mrlaoliai（小红书、抖音、TikTok、X 等）
- 商务与联系邮箱：polarisagi.online@gmail.com
- 设计讨论：GitHub Discussions
- Bug：GitHub Issues
- 安全漏洞：见 [SECURITY.md](./SECURITY.md) 或发邮件至 polarisagi.online@gmail.com，**不要**走公开 Issue

## 7. 许可

提交即视为同意按 [Apache-2.0](./LICENSE) 贡献。如代表雇主提交，请确保已获授权。
