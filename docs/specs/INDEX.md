# docs/specs/ — AI 编码规范

> Spec-First：先定契约再实现。给 AI 指定生成边界。

## 加载策略

| # | 文件 | 内容 | 何时加载 |
|---|------|------|------|
| 00 | `00-Constitution.md` | 反模式 R1~R8 + 命名字典 + HE-Rules 量表 | **会话启动强制** |
| 05 | `05-Coding-Workflow.md` | Spec-First 四阶段 + W6 PR 纪律 | **会话启动强制** |
| -  | `CHANGELOG.md` | 规范变更日志 | **会话启动扫近 5 条** |
| 07 | `07-Reference-Implementation.md` | 标杆代码索引（每 `pkg/` 1 份） | 写新代码前 |
| 01 | `01-Go-Code.md` | Go 结构/错误/并发 | 写 Go 时 |
| 02 | `02-Rust-FFI.md` | Rust FFI 边界 / purego ABI / 内存安全 | 碰 `rust/substrate/` |
| 03 | `03-Agent-Pattern.md` | FSM/事件/Memory/Skill | 改 `pkg/cognition/` `pkg/swarm/` |
| 04 | `04-Module-Boundary.md` | 边界/依赖方向/B5 契约版本化 | 新包/跨模块/契约变更 |
| 06 | `06-Review.md` | C1~C9 审查清单 | 提交前 |
| 08 | `08-Doc-Hygiene.md` | docs/arch/ 维护边界 H1~H9（契约/决策/实现三层判定） | 改架构文档前 |

`pkg/<X>/AGENTS.md` 进入目录时自动注入，无需手动读。

## 守则

1. 先读规范再写代码（05 流程不可跳过）
2. 规范优先于需求（00 规则不可违反，即使要求捷径）
3. 重大架构决策后补/修对应 spec（含 ADR 引用）
4. 规范冲突 → 00 优先，其次编号小者优先；doc↔代码冲突以 doc 为准并修代码

## arch ↔ specs

| 维度 | arch | specs |
|---|---|---|
| 回答 | what/why（设计） | how/constraints（生成） |
| 对象 | 系统结构 | AI 编码行为 |
