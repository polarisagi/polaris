# ADR-0033: M05 记忆子系统架构决策合集（写路径、范围限制、核心记忆与 MemFS、时序检索、热路径压缩）

> 状态：Accepted ｜ 首次：2026-06-13 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0023、0035、0036、0060、0082 ｜ 模块：M04 / M05 / M09

## 旧引用映射

| 旧引用 | 新节 |
|---|---|
| 0033 决策一 / 0023 | 决策一 |
| 0033 决策二 | 决策二 |
| 0033 决策三 / 0036 | 决策三 |
| 0033 决策四 / 0035 | 决策四 |
| 0033 决策五 / 0060 | 决策五 |
| 0082 全文（MemFS） | 决策六 |

## 决策一：episodic 写路径双轨制（原 ADR-0023）

- 热路径：`EpisodicMem.Append()` 同步写 `kv_store`；Payload >8KB 截断落盘至 `logs/events/<id>.bin`（workspace_vfs 相对路径），替换为含摘要的 `log_ref` 占位符；写入后同步向 outbox 投递 `target_engine="episodic"`。
- 冷投影：`EpisodicProjectorHandler`（OutboxWorker 异步消费）幂等 INSERT `episodic_events`；1 小时前且未被固化的事件由 `MarkColdEpisodicEvents` 标 `cold`，`OnlineReindexer` 跳过冷事件的 embedding。
- 反例：`EpisodicMem.Append` 直接双写两表（破坏 MutationBus 单写者约束；`episodic_events` 是投影派生表，写入必须经 OutboxWorker 保证幂等）。

## 决策二：不做列表（Won't Do）

- SurrealKV O(1) 技能签名双轨检索：SQLite Registry+Selector 规模内性能充足，双轨引入状态同步复杂度无收益。复议条件：压测证明其瓶颈。
- L3/L4 进化多签审批：单用户本地场景无意义，已由"全量回归+影子部署报告+强制冷却期"单人审批替代（ADR-0025 决策二 K — ShadowExecutor）。复议条件：出现真实多用户/多操作者场景。

## 决策三：核心工作记忆区 ZoneCoreMemory（原 ADR-0036）

- `PromptBuilder` 的 `ZoneCoreMemory` + `core_memory_edit` 工具，跨 session 持久化人物设定/任务状态/用户偏好。
- 结构化键值块集合（防意外覆盖）；简单 `set`/`append`/`delete` 语义（非 JSON Patch）；持久化到 `core_memory_blocks`（`034_core_memory.sql`，HE-6）；单块 2KB / 总量 8KB 硬上限；写入保留执行上下文污点级别；必须经标准 PolicyGate（HE-7）。
- 反例：核心记忆纯内存不落库；跳过 PolicyGate 直写。

## 决策四：时序记忆检索与信念修正（原 ADR-0035）

- `memory_search` 的 `as_of` 参数：按 `valid_from`/`valid_until` 过滤，BM25/向量/图三路透明适用。
- `ExclusiveWriter`：`user_preference` 实体 Jaccard >0.6 时自动 `MarkEntitySuperseded`，不直接覆写（历史不可变）。
- 反例：直接覆写历史记忆条目；`user_preference` 无 Jaccard 判断直接 UPSERT。

## 决策五：M4 ContextWindowManager 热路径压缩接入（原 ADR-0060）

- 抽出 `internal/memory/compact`（L1）：Stage1（tool_result 卸载）/ Stage2（LLM 摘要）/ Stage3（画布注入）为纯函数，只依赖 `protocol.Provider` + 窄接口 `compact.Offloader`；网关 `chat.Compressor` 委托调用，只保留持久化回写/hook/thrashing 计数。
- `Agent.hotPathCompactIfNeeded` 在每次组装 `reqMsgs` 后调用：软触发（>70%）只做 Stage1（"大 tool_result 优先卸载"作 salience 排序的保守替代）；硬触发（>90%）追加 Stage2+3。ReplayMode 物理短路。
- 压缩只作用于缓存前缀（最后一个 `CacheBreakpoint` 及其前 + 开头连续 system）之后的内容，L3 的 system 消息原样保留（ADR-0105 决策三/四）。
- 与 M4 任务级 token 预算三级检测（50/75/100%）互补：前者管任务累计预算，后者管单次请求消息大小。
- 复议条件：Stage1 保守替代长期无法逼近真实 salience 排序效果。

## 决策六：MemFS——扩展 core_memory_edit 实现显式可编程记忆块（原 ADR-0082）

- 以扩展既有 `core_memory_edit` 的 operation 集合实现，枚举 `["list","get","set","append","replace","delete","describe"]`，不新建独立工具族。
- `protocol.CoreMemory` 接口新增 `Replace`（块内精确子串替换，`old_str` 须唯一匹配，除非 `replace_all=true`）与 `Describe`；`Get`/`Set`/`Delete`/`List` 签名不变。
- `core_memory_blocks` 新增列：`description`、`read_only`（1=拒绝一切写）、`max_bytes`（单块上限，建块时由阈值 `core.memory_block_max_kb` 固化，不追溯已存在行）。
- 容量分层：单块走每行 `max_bytes`；总量沿用 `core.memory_total_max_kb`（不重复 SSoT）；块数上限 `core.memory_max_blocks`（`state.yaml §m5_memory`，16）。
- 污点 only-up：`replace`/`append` 后 `taint_level = max(原值, 入参污点)`，入参污点取 `S_VALIDATE` 注入的授信 `TaintLevel`（`protocol.CtxTaintLevelKey`），不采信 LLM 参数。
- `list` 返回不含 content（只含 `key/description/size_bytes/max_bytes/read_only/updated_at`）；全部 operation 经 `sandbox.InProcessFn` → `ExecuteTool`，无旁路。
- 已知缺口（2026-10-12 代码核实仍成立）：`read_only` 有强制与写路径拒绝，但无系统侧创建保护块的生产调用点；出现真实需求时先补产出方，不长期悬空。
- 反例：MemFS 做成通用 KV（那是 `memory_write` 的边界）；operation 绕过 `ExecuteTool`；`list` 返回内容；用 `updated_at` 冒充容量信息。`list` 不返回 content 的边界仅在出现无法经 `get` 取内容的场景时才重议。

## 已驳方案

| 方案 | 驳回理由 |
|---|---|
| 新建 `memfs_*` 独立工具族（对齐 Letta 5 个工具） | 与 `core_memory_edit` 语义重叠，徒增工具目录体积与选择负担（0082） |
| `max_bytes` 全部动态读全局配置（不落列） | 配置调整会静默改变已存在块的容量契约，无法按块差异化（0082） |
| `list` 返回全部 content | 与减少 token 放大目标冲突（0082） |

## 沿革

| 日期 | 变更 | 原编号 |
|---|---|---|
| 2026-06-13~07-02 | 双轨写路径、时序检索、核心记忆区、热路径压缩接入 | 0023/0035/0036/0060 |
| 2026-07-22 | 删除"有效窗"辅助函数与零生产构造点的 `SynapticPlasticityManager` | ADR-0062 |
| 2026-08-02 | MemFS 落地（阶段05 P-01） | 0082 |
| 2026-10-12 | 0082 并入；订正：冷事件标记规则改为"1 小时前且未固化"；热路径压缩固定前缀扩为缓存前缀；引用路径按代码核实 | — |

## 引用代码

- `internal/memory/store/episodic_mem.go`、`internal/memory/consolidation/episodic_projector.go`、`internal/memory/consolidation/consolidation.go`（`MarkColdEpisodicEvents`）、`internal/memory/graph/episodic_graph_bridge.go`、`internal/memory/retrieval/online_reindexer.go`
- `internal/memory/retrieval/exclusive_closure.go`（`ExclusiveWriter`）、`internal/memory/retrieval/retriever.go`（`HybridRetriever`）
- `internal/memory/store/core_memory.go`、`internal/tool/builtin/core_memory_edit.go`、`internal/tool/builtin/core_memory_edit_exec.go`、`internal/protocol/interfaces_memory.go`、`internal/protocol/schema/034_core_memory.sql`
- `internal/memory/compact/`、`internal/agent/agent_context_compaction.go`
- `docs/arch/spec/state.yaml §m5_memory`
