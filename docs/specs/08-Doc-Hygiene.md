# 08 文档卫生规约（docs/arch/ 维护边界）

> 对象：`docs/arch/M01~M13.md` + `M13-bis-Extension-Registry.md` + `00-Global-Dictionary.md` + `ARCHITECTURE.md`。目的：在 Token 预算与契约完整性之间维持稳态；AI 改架构文档不得违反本规约。
> 范围不含：`docs/arch/decisions/`（ADR 档案，独立维护）、`docs/arch/spec/state.yaml`（受 ADR-0006 治理）。

## H1 三层判定模型

| 层 | 定义 | 处置 |
|---|---|---|
| **契约层** | 跨模块顺序约束、状态转移表、不变量速查、跨模块契约表、信任分层关系数字 | **禁止压缩**（Tier C 禁区） |
| **决策层** | why-do（该实现存在的唯一动因）、被驳方案 why-not | why-do 留 M_X；why-not 迁 `decisions/ADR-XXXX.md`，M_X 仅留 `→ ADR-XXXX` 锚点 |
| **实现层** | 无跨模块顺序约束的步骤描述、注册细节、路由列表 | 可 EntryPoint 化（H4） |

判定优先级：契约层 > 决策层 > 实现层；一段兼具多层 → 按高层处置。

## H2 修饰物清理（强制）

删除：章节首行"重申职责"句（与 §0-bis 表重复）；散文式介绍段（"我们引入 X 来解决 Y"，除非属决策层 why-do）；同义副词堆叠、感叹修饰、过渡句。保留：定义、契约、表格、列表、代码锚点、不变量编号。

反例：「为了应对复杂推理和长程任务的中间步奖励需求，系统引入基于硬件感知的双轨评分机制」→ 删除，直接进入字段定义。

## H3 数值双写消除（强制）

**下推 `spec/state.yaml`**：纯阈值（MaxReplanAttempts、KillSwitch 阶段阈值、超时秒数、心跳间隔）及已在 state.yaml 定义的常量。引用范式：`MaxReplanAttempts (spec/state.yaml §m4_kernel.max_replan_attempts)`，M_X 内不再写具体数值。

**禁止下推**（留 M_X）：信任分层关系数字（如 M07 §4.3 Builtin/User/LLMGenerated 配额 256/128/64 MB）；Tier 0/1/2/3 内存阶梯数字（8/16/24/64 GB）；运行时可调旋钮的默认值（如 `PRMConfig.MaxCandidates=3`）。

判定边界：数字背后是否承载语义关系——是 → 留；否 → 下推。

门控状态：state.yaml 漂移由 `internal/protocol/spec_consistency_test.go` 守护（taint / par 状态与迁移 / kill_switch / task 与 outbox 状态 ↔ Go 枚举）；`internal/protocol/spec_header_counts_test.go` 校验 state.yaml 头注释状态数 = 实际列表长度，`internal/agent/fsm/fsm_spec_test.go` 校验 FSM 实现 ↔ state.yaml `par`。**只覆盖部分键，新增阈值须同步补断言**；未覆盖的键靠 H8 人工核对。

## H4 EntryPoint 化（分文件 review）

**前置条件**（缺一不可）：① 属实现层；② 不含 ≥2 个跨模块约束；③ 不含顺序敏感的安全/审计契约（PII、Taint、Capability 流转、EventLog 写入顺序）。满足 → 替换为 `**[EntryPoint]** internal/path/file.go:FunctionName (一句话功能语义)`。

可化：ToolRegistry 注册流水、SchemaManager DDL 注册、HTTP 路由列表、纯数据模型类型定义。
不可化：`S_VALIDATE` 四层校验（L0→L3 顺序 = 安全分层）；`ExecuteTool` 8 步（SecureUnredact 先于 EventLog 写入 = PII 单向击穿契约）；Crash Recovery 五段；状态机转移表（HE-5 强制可视）。不确定 → 默认保留。

## H5 决策迁移（分文件 review）

M_X 内"我们不用 X 因为 Y"：已含 `→ ADR-XXXX` 锚点 → 段落体迁 ADR，M_X 仅留锚点行；未含 ADR → 按 `decisions/ADR-template.md` 新建 ADR 再迁。M_X 仅保留 why-do（一句话）+ 锚点 `决策见 [ADR-XXXX](./decisions/ADR-XXXX-xxx.md)`。

## H6 Tier C 禁区

禁止修改（即使看起来冗长）：不变量速查表 `inv_MXX_NN`；§跳读 单行索引（`make docs-sync` 维护）；`INDEX.md` §2 场景表 / §2.5 章节级跳读 / §3 概念定位；跨模块契约表（§13 类）；顺序契约段（Step 0..N、五段流程、N 阶段管道）；ADR 档案体系；信任分层关系数字（H3）。

## H7 锚点化（巡检）

系统级名词强制带 `[]`，触发 AI 检索 `00-Global-Dictionary.md`：`[TaintLevel]` `[KillSwitch]` `[Cedar-Gate]` `[HE-Rule-N]` `[Tier-N-Limit]` `[Sandbox-LN]` `[Mem-LN]` `[Arch-LN]` `[Evo-LN]` `[HTN]` `[EventLog]` `[MutationBus]` `[Blackboard]` `[PromptBuilder]` `[ReplayMode]` `[Capability Token]` `[SurpriseIndex]` `[TokenBurnRate]`。代码块内（``` 或 ` `）原样保留。

## H8 验收门（每文件改完必跑）

1. **行动度量**：A1 修饰清理 ≥1 处 + H3 数值下推覆盖所有真双写（M_X 与 state.yaml 同名常量数值 100% 改为锚点引用），缺一 → fail。
2. **削契约防线**（单向下限，非目标）：字符变化超 **-30%** → fail；§0-ter（不变量）+ §跳读列出的契约段（§13 等）token 变化绝对值 **<5%**。
3. **不变量计数守恒**：`grep -roh "inv_M[0-9]*_[0-9]*" docs | sort -u | wc -l` 改动前后相等（当前基线 **80**）；删除任何 `inv_MXX_NN` → fail。
4. **锚点零失效**：`make docs-refs` pass——路径字面量、`.go` 注释路径、`M13 §1.2` 式章节锚点（`tools/anchor_refs.go`，baseline 当前 0）、ADR 编号体系（`tools/adr_index_check.go`）。
5. **生成块与源一致**：`make docs-gen-check` pass（`tools/docs_gen.go` 的 `BEGIN/END GENERATED` 块）。
6. **§跳读 行号同步**：`make docs-check` pass。

为什么不设百分比压缩预算：对着一份大半是不变量表与跨模块契约的文件，被要求"达标"的模型除了动规范性内容没有别的办法（Goodhart's law）。判据因此取"改动前后跑同一条命令比数字"的机械不变量（第 3~5 条），第 2 条只作防过度删除的上下限，不作目标。

## H9 Pilot 协议

新增改造范式时：① 选单一中等规模 M_X（避开最大的 M07 / M11）；② 跑 H8 全部；③ AI 实测：`INDEX.md §2` 中该模块对应 5 个场景，加载最小组合后能否答出"前置条件"；④ 全部 pass → 范式确认，批量推其余 M_X；任一 fail → 回退，修正本文档再 Pilot。首次 Pilot 为 M04。

---

`[Module-Topology]` `[HE-Rule-1]` 可观测优先 / `[HE-Rule-5]` 状态机持有控制流 — 本规约不可稀释这两条。
