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
| 0016 | 统一信任扩展模型（TrustTier 五级 + 官方 Publisher 白名单） | Accepted | 2026-05-21 |
| 0017 | MCP 默认传输层选 Streamable HTTP，SSE 降级 legacy | Accepted | 2026-05-21 |
| 0018 | MCP Transport 用 TaintPreservingDecoder，禁 encoding/json 直解 | Accepted | 2026-05-21 |
| 0019 | extension_instances 统一安装实例表（三层统一 State-in-DB） | Accepted | 2026-05-22 |
| 0020 | 确立 DeepSeek V4 为全系统默认核心模型（Canonical Provider） | Accepted | 2026-06-08 |
| 0021 | 核心机制实现（SurpriseIndex / ScriptTester / BM25 / FSM） | Accepted | 2026-06-09 |
| 0022 | ThinkingMode 三档路由取代 BestOfN/MCTS 多候选方案 | Accepted | 2026-06-13 |
| 0023 | episodic 写路径双轨制（kv_store 热路径 + OutboxWorker 冷投影） | Accepted | 2026-06-13 |
| 0024 | GovernanceAgent 代码安全三层防线（AST + 正则 + 单次 ThinkingMax LLM） | Accepted | 2026-06-13 |
| 0025 | 全局架构审查缺陷修复（R21，17 项 P0/P1/P2 缺陷） | Accepted | 2026-06-14 |
| 0026 | Logic Collapse Python Skill + ContainerSandbox 运行时 | Accepted | 2026-06-16 |
| 0027 | ~~Gemini 执行后遗留实现缺口修复~~ | Superseded → ADR-0025 | 2026-06-16 |
| 0028 | ~~Phase 0 P0 Bug 修复~~ | Superseded → ADR-0025 | 2026-06-17 |
| 0029 | Phase 1-2 系统加固（AgentPool / VFS 墓碑 / SQL Fitness / SafeGo 全量 / OS Fault 注入 / ShadowExecutor）| Accepted | 2026-06-17 |
| 0030 | Tier-2 语义嵌入升级（OpenAI 兼容 Embedding + Rust SIMD 余弦相似度）| Accepted | 2026-06-20 |
| 0031 | TTS 三路 Provider 架构（Edge / HTTP / Sherpa），默认 Edge TTS 中文高质量 | Accepted | 2026-06-27 |
| 0032 | — | 未分配/未使用 | — |
| 0033 | 记忆子系统范围限制与扩展综合决策（Won't Do + ZoneCoreMemory + 时序检索）| Accepted | 2026-07-02 |
| 0034 | ~~Tree-sitter CGO 例外授权~~ | Superseded → ADR-0011 | 2026-07-04 |
| 0035 | ~~时序记忆检索 + Jaccard 信念修正~~ | Superseded → ADR-0033 | 2026-07-09 |
| 0036 | ~~核心工作记忆区（ZoneCoreMemory）~~ | Superseded → ADR-0033 | 2026-07-09 |
| 0037 | — | 未分配/未使用 | — |
| 0038 | ~~影子执行器设计与异步回放选型~~ | Superseded → ADR-0029 | 2026-07-09 |
| 0039 | Gateway 控制权移交 FSM（废除 MVP 直通模式）| Accepted | 2026-07-08 |

> 现有 `docs/arch/M_X` 文档中的关键决策应回填为 ADR。回填优先级：依赖选型 > 跨层例外 > 性能权衡。

## 模板

见 [`ADR-template.md`](./ADR-template.md)。
