# 08 文档卫生规约（docs/arch/ 维护边界）

> 对象：`docs/arch/M01~M13.md` + `M13-bis-Extension-Registry.md` + `00-Global-Dictionary.md` + `ARCHITECTURE.md`。
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
`**[EntryPoint]** internal/path/file.go:FunctionName (一句话功能语义)`

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
- 不变量速查表 `inv_MXX_NN`（CLAUDE.md 强制项）
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
- `[EventLog]` `[MutationBus]` `[Blackboard]` `[PromptBuilder]`
- `[ReplayMode]` `[Capability Token]` `[SurpriseIndex]` `[TokenBurnRate]`

例外：代码块内（``` ``` 或 \` \`）原样保留。

巡检命令：`grep -P '(?<!\[)\b(KillSwitch|TaintLevel|Cedar-Gate|...)\b'` 仅命中代码块。

## H8 验收门（每文件改完必跑）

1. **行动度量**（首要判据）：A1 修饰清理 ≥1 处 + A2 数值下推覆盖所有真双写（M_X 与 state.yaml 同名常量数值 100% 改为锚点引用），缺一 → fail
2. **削契约防线**（单向下限，非目标）：字符变化超 **-30%** → fail；§0-ter（不变量）+ §跳读列出的契约段（§13 等）token 变化绝对值 **<5%**
3. **不变量计数守恒**：`grep -roh "inv_M[0-9]*_[0-9]*" docs | sort -u | wc -l` 改动前后相等（当前基线 **80**）。删除任何 `inv_MXX_NN` 条目 → fail
4. **§ 锚点零失效**：`make docs-refs` pass——含路径字面量、`.go` 注释路径、`M13 §1.2` 式章节锚点（`tools/anchor_refs.go`，baseline 存量当前为 0）、ADR 编号体系自洽（`tools/adr_index_check.go`）四项
5. **生成块与源一致**：`make docs-gen-check` pass（`tools/docs_gen.go` 维护的 `BEGIN/END GENERATED` 块）
6. **§跳读 行号同步**：`make docs-check` pass

> **2026-08-09 追记（推翻本节原第 1 条中的百分比预算表）**：原文在"行动度量"下列有按文件类型划分的 token 变化预算区间（契约密集型 -5%~+5% / 平衡型 -8%~-18% / 实现密集型 -15%~-25%），虽标注"参考值，非硬指标"，但对执行模型的牵引力远大于该免责标注——面对一份大半是不变量表与跨模块契约的文件，被要求"达标"的模型除了动规范性内容没有别的办法（Goodhart's law）。**已删除该预算表**，`-30%` 削契约防线与契约段 `<5%` 两条**保留并上提为第 2 条**：二者是防止过度删除的**下限/上限约束**，与"设定压缩目标"方向相反，不受本次推翻影响。
>
> 替代物是第 3~5 条的**机械不变量校验**：这三条在 2026-08-09 的文档优化终验中已实测有效（`inv_` 计数 80、`.go` 注释 § 引用 386 两项在跨越 6 个 commit、涉及 40+ 文件的改动前后精确相等，成功证明规范性内容零损伤）。判据从"事后估算百分比"改为"改动前后跑同一条命令比数字"，符合 HE-4 与本仓库门控优先的一贯取向。
>
> 依据：`local_playground/prompt/docs-optimization-plan.md §0-bis④`（原始论证）与 PS-009 登记。本条属规范性条款修订，按根 `CLAUDE.md §文档可修订性`"先改文档再改代码"执行，且已带门控证据（上述实测）。

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
