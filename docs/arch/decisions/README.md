# Architecture Decision Records (ADR)

> 记录非平凡架构决策。AI 修代码前必须 grep 相关 ADR，避免反复提议已被驳回的方案。

## 何时写 ADR

| 触发场景 | 示例 |
|---------|------|
| 依赖选型 | DB 引擎、库、外部服务 |
| 跨层例外 | 违反 B1 依赖方向的特批 |
| 性能权衡 | 牺牲可读性换性能、放弃通用性换 Tier-0 |
| 安全协议 | 新污点降级路径、新 sandbox 级别 |
| 反复询问 | "为什么不用 X" 已被多次询问 |

不需要 ADR 的：单纯的实现选择、可逆的局部决定、纯重构。

## 编号

按时间递增 4 位数字：`ADR-0001-<kebab-case-title>.md`

## 状态机

```
Proposed → Accepted ──→ Superseded by ADR-NNNN
                   ──→ Deprecated
```

## 引用纪律

ADR 被代码引用时，源文件头部加：

```go
// ADR: docs/arch/decisions/ADR-0001-sqlite-not-postgres.md
```

## 索引

| 编号 | 标题 | 状态 | 日期 |
|------|------|------|------|
| 0001 | observability 一等公民指标使用包级全局变量（R1.3 豁免） | Accepted | 2026-05-16 |
| 0002 | skill 子包内本地接口/类型消除（R1.4 合规） | Accepted（已执行完毕） | 2026-05-16 |
| 0003 | modernc/sqlite（零 CGO）作为主持久化存储 | Accepted（回填） | 2026-05-16 |
| 0004 | Tier-0 8GB 内存硬上限 + Hardware Tier 解锁机制 | Accepted（回填） | 2026-05-16 |
| 0005 | purego（零 CGO）作为 Go→Rust FFI 桥接方式 | Accepted（回填） | 2026-05-16 |
| 0006 | state.yaml 作为状态机 + 全模块阈值的 SSoT | Accepted（回填） | 2026-05-16 |
| 0007 | TaintLevel 五级 + 只升不降 + Sanitizer 受控降级 | Accepted（回填） | 2026-05-16 |
| 0008 | Sandbox 三级 + Tier-0 平台特化降级 | Accepted（回填） | 2026-05-16 |
| 0009 | KillSwitch 三阶段熔断 + `.fullstop` 持久状态 | Accepted（回填） | 2026-05-16 |
| 0010 | SurrealDB-Core（Rust FFI）作为认知检索轴 | Accepted（回填） | 2026-05-16 |
| 0011 | cgo → purego 迁移（cedar_ffi.go + surreal_store.go） | Accepted（已执行） | 2026-05-16 |
| 0012 | state.yaml ↔ Go 代码一致性回归测试设计 | Accepted（已执行） | 2026-05-16 |
| 0013 | lint 机械化 Phase 1（depguard / errorlint / nestif / gocyclo） | Accepted（已执行） | 2026-05-16 |
| 0014 | 对抗审查 GitHub Action（执行带 3） | Accepted（已执行） | 2026-05-16 |
| 0015 | Codex 特性集成（Plugin / Hooks / SKILL.md / Custom Agent / CSV fan-out） | Accepted | 2026-05-21 |
| 0016 | 统一信任扩展模型 | Accepted | — |
| 0017 | MCP 默认传输层选 Streamable HTTP，SSE 降级 legacy | Accepted | 2026-05-21 |
| 0018 | MCP Transport 用 TaintPreservingDecoder，禁 encoding/json 直解 | Accepted | 2026-05-21 |

> 现有 `docs/arch/M_X` 文档中的关键决策应回填为 ADR。回填优先级：依赖选型 > 跨层例外 > 性能权衡。

## 模板

见 [`ADR-template.md`](./ADR-template.md)。
