# 08 文档卫生规约（docs/arch/ 维护边界）

> 对象：`docs/arch/M01~M13.md` + `00-Global-Dictionary.md` + `ARCHITECTURE.md`。
> 目的：在 Token 预算与契约完整性之间维持稳态。AI 改架构文档时不可违反本规约。
> 范围不含：`docs/arch/decisions/`（ADR 档案，独立维护）、`docs/arch/spec/state.yaml`（受 ADR-0006 治理）。

## H1 三层判定模型

每段文字落于以下三层之一，处置规则不同。

| 层 | 定义 | 处置 |
|---|---|---|
| **契约层** | 跨模块顺序约束、状态转移表、不变量速查、跨模块契约表、信任分层关系数字 | **禁止压缩**（Tier C 禁区） |
| **决策层** | why-do（该实现存在的唯一动因）、被驳方案 why-not | why-do 留 M_X；why-not 迁 `decisions/ADR-XXXX.md`，M_X 仅保留 `→ ADR-XXXX` 锚点 |
| **实现层** | 无跨模块顺序约束的步骤描述、注册细节、路由列表 | 可 EntryPoint 化（H4） |

判定优先级：契约层 > 决策层 > 实现层。一段同时含两类 → 按高层处置。

## H2 修饰物清理（Tier A1，强制）

删除以下文本，禁止保留：
- 章节首行"重申职责"句（与 §0-bis 表已述重复）
- 散文式介绍段（"我们引入 X 来解决 Y"型，除非属决策层 why-do）
- 同义副词堆叠、感叹修饰、过渡句

保留：定义、契约、表格、列表、代码锚点、不变量编号。

**反例**：「为了应对复杂推理和长程任务的中间步奖励需求，系统引入基于硬件感知的双轨评分机制」→ 删除。直接进入字段定义。

## H3 数值双写消除（Tier A2，强制）

**强制下推 `spec/state.yaml`** 的数值：
- 纯阈值（MaxReplanAttempts、KillSwitch 阶段阈值、超时秒数、心跳间隔）
- 已在 state.yaml 定义的常量

引用范式：`MaxReplanAttempts (spec/state.yaml §m4_kernel.max_replan_attempts)` —— 不再在 M_X 内写具体数值。

**禁止下推**（保留在 M_X）：
- 信任分层关系数字（如 M07 §4.3 Builtin/User/LLMGenerated 资源配额 256/128/64 MB——数字本身=规约）
- 表达 Tier 0/1/2/3 内存阶梯的数字（8/16/24/64 GB）
- 运行时可调旋钮的默认值（如 PRMConfig.MaxCandidates=3）

判定边界：**数字背后是否承载语义关系**。是 → 留；否 → 下推。

CI 校验：`make docs-check` 扩展 `lint_doc_state_yaml_drift`（脚本后置补齐），M_X 出现与 state.yaml 同名常量但数值不同 → fail。

## H4 EntryPoint 化（Tier B1，分文件 review）

**前置条件**（缺一不可）：
1. 该段属"实现层"
2. 该段不含 ≥2 个跨模块约束
3. 该段不含顺序敏感的安全/审计契约（PII、Taint、Capability 流转、EventLog 写入顺序）

满足 → 替换为：
`**[EntryPoint]** pkg/path/file.go:FunctionName (一句话功能语义)`

**典型可化场景**：ToolRegistry 注册流水、SchemaManager DDL 注册、HTTP 路由列表、纯数据模型类型定义。

**典型不可化场景**：
- `S_VALIDATE` 四层校验（L0→L1→L2→L3 顺序=安全分层）
- `ExecuteTool` 8 步（SecureUnredact 必须先于 EventLog 写入=PII 单向击穿契约）
- Crash Recovery 五段（PII/快照/恢复/幂等顺序=可重放性契约）
- 状态机转移表（HE-Rule-5 强制可视）

不确定 → 默认保留，不化。

## H5 决策迁移（Tier B2，分文件 review）

M_X 内"我们不用 X 因为 Y"段落处置：
1. 已含 `→ ADR-XXXX` 锚点 → 段落体迁至 ADR，M_X 仅保留锚点行
2. 未含 ADR → 新建 ADR，按 `decisions/ADR-template.md` 起草，再做第 1 步

M_X 内仅保留：
- why-do：该实现存在的唯一动因（一句话）
- ADR 锚点：`决策见 [ADR-XXXX](./decisions/ADR-XXXX-xxx.md)`

## H6 Tier C 禁区

**禁止修改**以下结构（即使看起来冗长）：
- 不变量速查表 `inv_MXX_NN`（AGENTS.md 强制项）
- §跳读 单行索引（由 `make docs-sync` 维护）
- INDEX.md §2 场景表、§2.5 章节级跳读、§3 概念定位
- 跨模块契约表（§13 类）
- 顺序契约段（Step 0..N、五段流程、N 阶段管道）
- ADR 档案体系
- 信任分层关系数字（见 H3）

## H7 锚点化（Tier A3，巡检）

系统级名词强制带 `[]`，触发 AI 检索 `00-Global-Dictionary.md`：
- `[TaintLevel]` `[KillSwitch]` `[Cedar-Gate]` `[HE-Rule-N]` `[Tier-N-Limit]`
- `[Sandbox-LN]` `[Mem-LN]` `[Arch-LN]` `[Evo-LN]` `[HTN]`
- `[EventLog]` `[MutationBus]` `[Blackboard]` `[ContextAssembler]`
- `[ReplayMode]` `[Capability Token]` `[SurpriseIndex]` `[TokenBurnRate]`

例外：代码块内（``` ``` 或 \` \`）原样保留。

巡检命令：`grep -P '(?<!\[)\b(KillSwitch|TaintLevel|Cedar-Gate|...)\b'` 仅命中代码块。

## H8 验收门（每文件改完必跑）

1. **行动度量**（首要判据，取代字符变化率）：
   - A1 修饰清理 ≥1 处 + A2 数值下推覆盖所有真双写（M_X 与 state.yaml 同名常量数值 100% 改为锚点引用），缺一 → fail
   - 文件类型 token 变化预算（参考值，非硬指标）：
     - 契约密集型（契约层占比 ≥70%，如 M04/M11）：-5%~+5%（A2 下推导致路径名比数字长，允许微增）
     - 平衡型（M01/M03/M05/M06/M08/M09/M10/M12）：-8%~-18%
     - 实现密集型（M02/M07/M13）：-15%~-25%
   - 字符变化超 -30%（疑似削契约）→ fail
2. **契约章节 token**：§0-ter（不变量）+ §跳读列出的契约段（§13 等）token 变化绝对值 <5%
3. **inv_MXX_NN 完整**：删除任何条目 → fail
4. **ADR 引用 0 断裂**：`grep -oE 'ADR-[0-9]{4}' M_X.md` 全部在 `decisions/` 存在（原文未引用 ADR 视为 N/A，不计 fail）
5. **§跳读 行号同步**：`make docs-check` pass

## H9 Pilot 协议

新增改造范式时：
1. 选**单一中等规模 M_X**（避开最大的 M07 / M11）
2. 跑 H8 五条
3. 跑 AI 实测：INDEX §2 中该模块对应 5 个场景，加载最小组合后能否答出"前置条件"
4. 全部 pass → 范式确认，可批量推其余 M_X
5. 任一 fail → 范式回退，分析后修正本文档再 Pilot

首次 Pilot 选 M04。

---

`[Module-Topology]` `[HE-Rule-1]` 可观测优先 / `[HE-Rule-5]` 状态机持有控制流 — 本规约不可稀释这两条。
