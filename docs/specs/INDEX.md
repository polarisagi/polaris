# docs/specs/ — AI 编码规范

> Spec-First：先定契约再实现，给 AI 指定生成边界。

## 加载策略

| # | 文件 | 内容 | 何时加载 |
|---|------|------|------|
| 00 | `00-Constitution.md` | 反模式 R1~R8 + 命名字典 + HE-1~7 量表 | **会话启动强制** |
| 05 | `05-Coding-Workflow.md` | W0 排障 + Spec-First 四阶段 + W6 PR 纪律 + W7 Schema 流程 + W8 多代理协作 | **会话启动强制** |
| -  | `CHANGELOG.md` | 规范变更日志 | **会话启动扫近 5 条** |
| 07 | `07-Reference-Implementation.md` | 标杆代码索引（每功能域 1 份） | 写新代码前 |
| 01 | `01-Go-Code.md` | Go 结构/错误/并发/日志/Handler/Context | 写 Go 时 |
| 02 | `02-Rust-FFI.md` | Rust FFI 边界 / purego ABI / 内存安全 | 碰 `rust/substrate/` |
| 03 | `03-Agent-Pattern.md` | FSM/事件/Memory/Skill/TrustTier/CodeAct | 改 `internal/agent/` `memory/` `swarm/` |
| 04 | `04-Module-Boundary.md` | 层依赖方向 B1 / 契约版本化 B5 | 新包/跨模块/契约变更 |
| 06 | `06-Review.md` | C1~C11 审查清单 | 提交前 |
| 08 | `08-Doc-Hygiene.md` | `docs/arch/` 维护边界 H1~H9 | 改架构文档前 |
| 09 | `09-LLM-Agent-Production.md` | A-01~A-14 陷阱 + P-1~P-9 原则 + RAG/并发检查清单 | 写任何 Agent/LLM/Tool/RAG/Memory 代码前 |

`internal/<X>/CLAUDE.md` 进入目录时必读（Claude Code 自动注入，其他 AI 工具需手动加载）。

## 守则

1. 先读规范再写代码（05 流程不可跳过）
2. 规范优先于需求（00 规则不可违反，即使要求捷径）
3. 重大架构决策后补/修对应 spec（含 ADR 引用）
4. 规范冲突 → 00 优先，其次编号小者优先。文档与代码冲突按根 `CLAUDE.md §文档可修订性` 的两类模型判定：**规范性**文档（HE-1~7、`specs/` R1~R8 + R2 命名、`state.yaml` 阈值、`00-Global-Dictionary` 概念、DDL）以文档为准、改代码；**描述性**文档（`arch/` 的实现描述、ADR 事实引用）以代码为准、订正文档

## arch ↔ specs

`arch/` 回答 what/why（系统结构的设计）；`specs/` 回答 how/constraints（AI 编码行为的生成约束）。
