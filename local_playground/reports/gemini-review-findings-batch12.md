# Gemini Review Findings - Batch 12 (docs/ 快速自审)

## 批次 12 汇总表

| ID | 严重级/动作 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-12-001 | P2 | docs/arch (L0) | M02 §16 DDL 全量文件数与末尾编号与 schema 现状漂移 | 高 | 是 (建议规则: grep -n "028_apps" docs/arch/M02-Storage-Fabric.md 提取并核对 DDL 文件总数及上限) |
| GR-12-002 | P2 | docs/arch (L1) | M04 §1 状态枚举声明列表缺漏且权威源文件名标注错误 | 高 | 是 (建议规则: grep -n "AgentState:" docs/arch/M04-Agent-Kernel.md 校验状态枚举列表完整性与权威定义文件) |
| GR-12-003 | P2 | docs/arch (契约) | INDEX §1 场景加载预算中多份核心文档 est_tok 估算严重低估 | 高 | 是 (建议规则: 校验 docs/arch/INDEX.md §1 表中 est_tok 估算值与实际文件 Byte/Token 的比例漂移) |
| GR-12-004 | P2 | docs/specs (契约) | 宪法 R2.5 错误码字典缺失 CodeStorageUnavailable 权威定义 | 高 | 是 (建议规则: grep -n "Code.*Code = " pkg/apperr/apperr.go 与 docs/specs/00-Constitution.md §R2.5 错误码列表逐一求差集) |
| GR-12-005 | P2 | docs/arch (L0) | 全局字典 §12 追溯表中各模块 DDL 文件范围陈旧 | 高 | 是 (建议规则: grep -n "001-006_\*\.sql" docs/arch/00-Global-Dictionary.md 提取并校验 DDL 范围) |
| GR-12-006 | P2 | docs/arch (L0) | M02 §2 tasks 表结构列说明遗漏 spawn_depth 字段 | 高 | 是 (建议规则: 提取 docs/arch/M02-Storage-Fabric.md §2 tasks 表列定义与 internal/protocol/schema/007_tasks.sql 列定义求差集) |
| GR-12-007 | P2 | docs/arch (L3) | M13-bis §2.2 表格中 origin 枚举值与 DDL/类型定义自相矛盾 | 高 | 是 (建议规则: grep -n "'official'" docs/arch/M13-bis-Extension-Registry.md 校验 origin 枚举列表内部一致性) |

置信度分布声明: 全部 7 条发现经过源码/DDL 物理文件交叉核对及 §2-A 反证，证据行真实可见且无须假定运行时条件，故均判定为高置信度。

---

### [GR-12-001] M02 §16 DDL 全量文件数与末尾编号与 schema 现状漂移
- 严重级: P2
- 模块: docs/arch（层: L0）
- 位置: docs/arch/M02-Storage-Fabric.md:433
- 违反规则: docs↔code 漂移
- 置信度: 高
- 可机械化: 是 (建议规则: grep -n "028_apps" docs/arch/M02-Storage-Fabric.md 提取并核对 DDL 文件总数及上限)
- 反证: 已核对 internal/protocol/schema/ 物理文件，确认文件数确实为 35 份，028~038 段包含 029_workflows.sql 至 038_idempotent_cache.sql 10 个新增建表文件；根 CLAUDE.md 已标注 35 份，但 M02-Storage-Fabric.md 仍遗留历史 25 份快照，未及时更新。
- 问题: M02-Storage-Fabric.md §16 表中声明 "全部 DDL（001_events 至 028_apps，共 25 份，权威目录 internal/protocol/schema/）"。而实际上 internal/protocol/schema/ 目录中共有 35 个 SQL 文件（001~024，028~038，其中 025~027 为刻意预留跳号），末尾文件已延伸至 038_idempotent_cache.sql。文档在 DDL 文件总量（25 vs 35）及末尾落点（028_apps vs 038_idempotent_cache）上与代码 SSoT 均严重脱节。
- 证据: 如下
  ```text
  文档: docs/arch/M02-Storage-Fabric.md:433
  | DDL | 全部 DDL（001_events 至 028_apps，共 25 份，权威目录 `internal/protocol/schema/`） | internal/protocol/schema/ |
  代码: ls internal/protocol/schema/*.sql -> 001_events.sql ... 038_idempotent_cache.sql (共 35 份)
  ```
- 修复方向提示: 将 M02-Storage-Fabric.md:433 的 "001_events 至 028_apps，共 25 份" 更新为 "001_events 至 038_idempotent_cache，共 35 份（025~027 保留）"。

### [GR-12-002] M04 §1 状态枚举声明列表缺漏且权威源文件名标注错误
- 严重级: P2
- 模块: docs/arch（层: L1）
- 位置: docs/arch/M04-Agent-Kernel.md:46
- 违反规则: docs↔code 漂移
- 置信度: 高
- 可机械化: 是 (建议规则: grep -n "AgentState:" docs/arch/M04-Agent-Kernel.md 校验状态枚举列表完整性与权威定义文件)
- 反证: 已核对 pkg/types/enums_agent.go 与 internal/protocol/types.go，确认 AgentState 类型及其 13 个常数（含 AgentStateSuspended 和 AgentStateAwaitAgent）定位于 pkg/types/enums_agent.go，internal/protocol/types.go 中仅有结构体字段引用该类型；M04 本身第 62 行也已按 13 态阐述，证明第 46 行属早期遗留未同步的矛盾陈述。
- 问题: M04-Agent-Kernel.md §1 第 46 行标注 "状态枚举权威定义见 internal/protocol/types.go (AgentState: Idle/Perceive/Plan/Validate/Execute/Reflect/Replan/Rollback/Interrupt/Complete/Failed)"。此段存在两处漂移：① 状态列表仅列出 11 态，漏掉了 Suspended 与 AwaitAgent，与同一文件第 62 行明确声明的 "共 13 态" 以及 state.yaml:32 矛盾；② 将全系统唯一权威定义文件标注为 internal/protocol/types.go，而实际代码规范 SSoT 文件已收敛至 pkg/types/enums_agent.go:14。
- 证据: 如下
  ```text
  文档: docs/arch/M04-Agent-Kernel.md:46
  状态枚举权威定义见 `internal/protocol/types.go` (AgentState: Idle/Perceive/Plan/Validate/Execute/Reflect/Replan/Rollback/Interrupt/Complete/Failed)。
  代码: pkg/types/enums_agent.go:14-30 (AgentStateIdle ... AgentStateAwaitAgent 共 13 个常数)
  ```
- 修复方向提示: 将第 46 行中的 internal/protocol/types.go 改为 pkg/types/enums_agent.go，并将括号内枚举补齐为 13 态全集。

### [GR-12-003] INDEX §1 场景加载预算中多份核心文档 est_tok 估算严重低估
- 严重级: P2
- 模块: docs/arch（层: 契约）
- 位置: docs/arch/INDEX.md:101
- 违反规则: SSoT-L1
- 置信度: 高
- 可机械化: 是 (建议规则: 校验 docs/arch/INDEX.md §1 表中 est_tok 估算值与实际文件 Byte/Token 的比例漂移)
- 反证: 已核对 wc -c docs/arch/*.md 的物理字节大小，确认 M13-bis、00-Dict、M13、M05、M07 等文档在经历多轮架构重构与功能回写后体积已大幅膨胀，但 INDEX.md §1 的 est_tok 仍保留了初期的快照估算值，未随文档演进同步刷新。
- 问题: docs/arch/INDEX.md §1 "文档清单" 表中给出的文档 token 体量估算（est_tok）与实际文件体积发生严重偏差。例如：M13-bis-Extension-Registry.md 标注为 5K，实测文件 33.3KB（约 25K~30K token，低估达 6.6 倍）；00-Global-Dictionary.md 标注为 23K，实测 51.4KB（约 45K~50K token）；M13-Interface-Scheduler.md 标注为 28K，实测 76.5KB（约 65K~70K token）。偏差导致 AI 编程在依据 §2 评估场景加载预算时严重误判上下文开销，引发 token 溢出或装载超限。
- 证据: 如下
  ```text
  文档: docs/arch/INDEX.md:101-107
  | `M13-Interface-Scheduler.md` | L3 接口 | 28K |
  | `M13-bis-Extension-Registry.md` | L3 扩展 | 5K |
  | `00-Global-Dictionary.md` | 字典 | 23K |
  实际文件大小: docs/arch/M13-bis-Extension-Registry.md (33,351 bytes), docs/arch/00-Global-Dictionary.md (51,428 bytes)
  ```
- 修复方向提示: 重新计算并更新 docs/arch/INDEX.md §1 表中各文档的 est_tok 估算值。

### [GR-12-004] 宪法 R2.5 错误码字典缺失 CodeStorageUnavailable 权威定义
- 严重级: P2
- 模块: docs/specs（层: 契约）
- 位置: docs/specs/00-Constitution.md:101
- 违反规则: docs↔code 漂移
- 置信度: 高
- 可机械化: 是 (建议规则: grep -n "Code.*Code = " pkg/apperr/apperr.go 与 docs/specs/00-Constitution.md §R2.5 错误码列表逐一求差集)
- 反证: 已核对 pkg/apperr/apperr.go 第 50 行，确认 CodeStorageUnavailable 已存在且在 internal/agent/agent_execute_util.go 中被生产代码引用；而宪法 §R2.5 的表格中未收录此码。
- 问题: docs/specs/00-Constitution.md §R2.5 声明 "权威源：pkg/apperr/apperr.go（唯一定义处，禁止在其他包新增 Code 常量）"，但在其列出的错误码表中，缺失了 pkg/apperr/apperr.go:50 定义的 CodeStorageUnavailable（持久化存储层故障熔断专用错误码，由 GD-13-003 / ADR-0046 引入）。规范文档未覆盖完整错误码枚举。
- 证据: 如下
  ```text
  文档: docs/specs/00-Constitution.md:101-109
  | 资源 | `CodeNotFound`, `CodeAlreadyExists`, `CodeConflict`, `CodeResourceExhausted` |
  代码: pkg/apperr/apperr.go:50
  CodeStorageUnavailable Code = "STORAGE_UNAVAILABLE"
  ```
- 修复方向提示: 在 docs/specs/00-Constitution.md §R2.5 的表格中补齐 CodeStorageUnavailable 项。

### [GR-12-005] 全局字典 §12 追溯表中各模块 DDL 文件范围陈旧
- 严重级: P2
- 模块: docs/arch（层: L0）
- 位置: docs/arch/00-Global-Dictionary.md:628
- 违反规则: docs↔code 漂移
- 置信度: 高
- 可机械化: 是 (建议规则: grep -n "001-006_\*\.sql" docs/arch/00-Global-Dictionary.md 提取并校验 DDL 范围)
- 反证: 已查 internal/protocol/schema/ 目录，确认架构 DDL 远不止 001-006，全仓 35 份 SQL 文件均包含架构定义与字段注释；此处 "001-006_*.sql" 属早期字典初稿留下的陈旧范围，未同步扩展。
- 问题: docs/arch/00-Global-Dictionary.md §12 [标签→实现文件追溯] 表中第 628 行标注 "各模块 DDL: internal/protocol/schema/001-006_*.sql (架构 DDL 含中文注释)"。此处将全仓 DDL 范围严重限制在历史前 6 份文件（001-006），未能体现现行 35 份 DDL Schema 文件（001_events 至 038_idempotent_cache）的现状，存在明显文档陈旧漂移。
- 证据: 如下
  ```text
  文档: docs/arch/00-Global-Dictionary.md:628
  | 各模块 DDL | `internal/protocol/schema/001-006_*.sql` (架构 DDL 含中文注释) |
  代码: ls internal/protocol/schema/*.sql -> 001_events.sql ... 038_idempotent_cache.sql (共 35 份)
  ```
- 修复方向提示: 将 00-Global-Dictionary.md:628 中的 001-006_*.sql 更新为 001-038_*.sql。

### [GR-12-006] M02 §2 tasks 表结构列说明遗漏 spawn_depth 字段
- 严重级: P2
- 模块: docs/arch（层: L0）
- 位置: docs/arch/M02-Storage-Fabric.md:178
- 违反规则: docs↔code 漂移
- 置信度: 高
- 可机械化: 是 (建议规则: 提取 docs/arch/M02-Storage-Fabric.md §2 tasks 表列定义与 internal/protocol/schema/007_tasks.sql 列定义求差集)
- 反证: 已查 internal/protocol/schema/007_tasks.sql 第 70 行与 internal/execute/orchestrator/sqlite_blackboard.go，确认 spawn_depth 已实际落盘与读取；M02 §2 声明覆盖所有历史迁移后的最终列，却未收录该列。
- 问题: docs/arch/M02-Storage-Fabric.md §2 给出 tasks 表的关键列说明（第 178 行声明 "以下为文档层声明，覆盖所有历史迁移后的最终列集合"）。但在随后的表格（第 180-208 行）中，漏掉了 internal/protocol/schema/007_tasks.sql:70 新增的 spawn_depth 列（TaskEntry.SpawnDepth 持久化落点，由 ADR-0084 引入，用于 transfer_to_agent 委派深度限制）。
- 证据: 如下
  ```text
  文档: docs/arch/M02-Storage-Fabric.md:178-208
  (表格列出 task_id, session_id ... cost_usd, created_at, updated_at，缺失 spawn_depth)
  代码: internal/protocol/schema/007_tasks.sql:70
  spawn_depth INTEGER NOT NULL DEFAULT 0,
  ```
- 修复方向提示: 在 M02-Storage-Fabric.md §2 tasks 表结构说明表格中补充 spawn_depth 列的定义与语义说明。

### [GR-12-007] M13-bis §2.2 表格中 origin 枚举值与 DDL/类型定义自相矛盾
- 严重级: P2
- 模块: docs/arch（层: L3）
- 位置: docs/arch/M13-bis-Extension-Registry.md:72
- 违反规则: SSoT-L1
- 置信度: 高
- 可机械化: 是 (建议规则: grep -n "'official'" docs/arch/M13-bis-Extension-Registry.md 校验 origin 枚举列表内部一致性)
- 反证: 已查 internal/protocol/schema/020_extension_instances.sql 与 internal/extension/ 模块相关代码，确认代码中 origin 仅处理 builtin / marketplace / user / learned，不存在 official 字符串枚举值；官方市场扩展在代码中均设为 origin = "marketplace"。§2.2 表格中的 official 属于概念混淆误写。
- 问题: docs/arch/M13-bis-Extension-Registry.md §2.2 表格第 72 行将 official 列为 origin 枚举值之一（"official: 官方市场推荐包, trust_tier 默认 3"）。然而：① 同一文件第 26 行与 020_extension_instances.sql:14-18 明确指明 origin 枚举仅有 4 个合法值（'builtin' | 'marketplace' | 'user' | 'learned'）；② 官方包在架构中由 origin='marketplace' + trust_tier=3（或 catalog_id 非空）表达，并非独立的 origin='official' 枚举。§2.2 的表格描述破坏了文档内部及 docs↔schema 的概念一致性。
- 证据: 如下
  ```text
  文档内部矛盾:
  M13-bis §1 (line 26): origin 枚举: builtin | marketplace | user | learned
  M13-bis §2.2 表格 (line 72): | official | 官方市场推荐包 | 3 TrustOfficial |
  DDL: internal/protocol/schema/020_extension_instances.sql:14
  -- origin 枚举: builtin | marketplace | user | learned
  ```
- 修复方向提示: 修正 M13-bis-Extension-Registry.md §2.2 表格中的 official 行，澄清官方包的实际表示为 origin='marketplace'（trust_tier=3）。

---

## 已审文件清单

- `docs/arch/ARCHITECTURE.md`
- `docs/arch/INDEX.md`
- `docs/arch/00-Global-Dictionary.md`
- `docs/arch/Module-Dependency-Axioms.md`
- `docs/arch/M01-Inference-Runtime.md`
- `docs/arch/M02-Storage-Fabric.md`
- `docs/arch/M03-Observability.md`
- `docs/arch/M04-Agent-Kernel.md`
- `docs/arch/M05-Memory-System.md`
- `docs/arch/M06-Skill-Library.md`
- `docs/arch/M07-Tool-Action-Layer.md`
- `docs/arch/M08-Multi-Agent-Orchestrator.md`
- `docs/arch/M09-Self-Improvement-Engine.md`
- `docs/arch/M10-Knowledge-RAG.md`
- `docs/arch/M11-Policy-Safety.md`
- `docs/arch/M12-Eval-Harness.md`
- `docs/arch/M13-Interface-Scheduler.md`
- `docs/arch/M13-bis-Extension-Registry.md`
- `docs/arch/spec/state.yaml`
- `docs/specs/00-Constitution.md`
- `docs/specs/INDEX.md`
- `docs/specs/09-LLM-Agent-Production.md`
- `docs/specs/CHANGELOG.md`

## 明确未覆盖的范围

- `docs/hooks-examples/`（外部 Shell Hooks 示例脚本，非核心架构规约）
- `docs/research/`（研究与试验性草案文档）
- `docs/arch/decisions/`（ADR 决策档案，其深度生命周期与兑现治理独立划归于文档轨道批次 8~9 覆盖）

## 审了但无发现的模块

- `docs/arch/M01-Inference-Runtime.md`
- `docs/arch/M03-Observability.md`
- `docs/arch/M06-Skill-Library.md`
- `docs/arch/M09-Self-Improvement-Engine.md`
- `docs/arch/M10-Knowledge-RAG.md`
- `docs/arch/M12-Eval-Harness.md`
- `docs/arch/M13-Interface-Scheduler.md`
- `docs/arch/ARCHITECTURE.md`
