# ADR-0033: M05 记忆子系统架构决策合集（写路径 + 范围限制 + 容量压缩，含原 ADR-0023/0035/0036/0060）

- **状态**: Accepted | **日期**: 2026-06-13~07-02（合并 2026-07-09/2026-07-28）| **模块**: M04/M05/M09

## 决策一：episodic 写路径双轨制（原 ADR-0023）

`episodic_events` 表原为纸面投影（无 INSERT 路径，向量检索永远为空）。建立双轨写路径并对 kv_store 加 8KB Payload 门控：

- **热路径**：`EpisodicMem.Append()` 同步写 `kv_store`；Payload 超 8KB 截断落盘至 `logs/events/<id>.bin`，替换为含摘要的 `log_ref` 占位符。写入后同步向 outbox 投递 `target_engine="episodic"`。
- **冷投影**：`EpisodicProjectorHandler`（OutboxWorker 异步消费）幂等 INSERT `episodic_events`；session 内 seq 距最大 seq 超 1000 的历史行标记 `cold=1`，`OnlineReindexer` 跳过冷事件的 embedding 生成。

禁止 `EpisodicMem.Append` 直接双写两表——破坏 MutationBus 单写者约束；`episodic_events` 是投影派生表，写入必须经 OutboxWorker 保证幂等。

## 决策二：不做列表（Won't Do）

- **SurrealKV O(1) 技能签名双轨检索**：现有 SQLite Registry+Selector 在当前规模性能充足，双轨引入状态同步复杂度无收益。压测证明瓶颈后方可重议。
- **L3/L4 进化多签审批**：单用户本地场景无意义，已由"全量回归+影子部署报告+强制冷却期"单人审批替代（见 [ADR-0025](./ADR-0025-global-audit-remediation.md) 决策二 K — ShadowExecutor）。

## 决策三：核心工作记忆区 ZoneCoreMemory（原 ADR-0036）

`PromptBuilder` 新增 `ZoneCoreMemory` + `core_memory_edit` 工具，跨 session 持久化人物设定/任务状态/用户偏好：
- 结构化键值块集合（非单一自由文本，防意外覆盖）
- `set`/`append`/`delete` 简单语义（非 JSON Patch，防 LLM 生成脆弱）
- 持久化到 `core_memory_blocks` 表（`034_core_memory.sql`），非纯内存（HE-6 State-in-DB）
- 单块 2KB / 总量 8KB 硬上限；写入保留执行上下文污点级别
- 必须经标准 PolicyGate（HE-7 防退化边界）

## 决策四：时序记忆检索与信念修正（原 ADR-0035）

- **AsOf 时序检索**：`memory_search` 新增 `as_of` 参数，命中解析时按 `valid_from`/`valid_until` 过滤，BM25/向量/图三路透明适用。
- **ExclusiveWriter 信念修正**：`user_preference` 实体 Jaccard 相似度 >0.6 时自动 `MarkEntitySuperseded`，非直接覆写（历史记录不可变）。

## 决策五：M4 ContextWindowManager 热路径压缩接入（原 ADR-0060）

M4 单次 LLM 调用消息大小防线（`ContextWindowManager`）与 M5 网关会话压缩（`chat.Compressor`）此前从未连接——非因表示不同（`pkg/types.Message` 是同一类型），而是算法与网关专属关注点（持久化回写/hook/thrashing 计数）耦合在同一结构体里。

新建 `internal/memory/compact`（L1）抽出 Stage1(tool_result 卸载)/Stage2(LLM 摘要)/Stage3(画布注入) 为纯函数，只依赖 `protocol.Provider` + 窄接口 `compact.Offloader`。`chat.Compressor` 重构为委托调用，只保留网关专属部分。`Agent` 新增 `cwm *ContextWindowManager`，`hotPathCompactIfNeeded` 在每次组装 `reqMsgs` 后调用：软触发(>70%)只做 Stage1（无 salience 排序实现，用"大 tool_result 优先卸载"作诚实的保守替代）；硬触发(>90%)追加 Stage2+3。ReplayMode 下物理短路。

M4 任务级 token 预算三级检测（50/75/100%）与本机制互补，非替代——前者管任务累计预算，后者管单次请求消息大小。

## 反例守护

拒绝直接覆写历史记忆条目；拒绝 `user_preference` 无 Jaccard 判断直接 UPSERT；拒绝核心记忆纯内存不落库；拒绝跳过 PolicyGate 直写核心记忆；拒绝 `EpisodicMem.Append` 双写两表。

## 引用代码

`internal/memory/store/episodic_mem.go`、`internal/memory/graph/episodic_graph_bridge.go`、`internal/memory/retrieval/online_reindexer.go`、`internal/memory/retrieval/exclusive_closure.go`（`ExclusiveWriter` 实际定义处，原引用路径 `consolidation/exclusive_writer.go` 不存在）、`internal/memory/retrieval/retriever.go`（`HybridRetriever` 实际定义处，原引用文件名 `hybrid_retriever.go` 不存在）、`internal/tool/builtin/core_memory_edit/`、`internal/protocol/schema/034_core_memory.sql`、`internal/memory/compact/`、`internal/agent/agent_context_compaction.go`

## 修订记录

2026-07-22：显式"有效窗"辅助函数与 `SynapticPlasticityManager`（零生产构造点）已删除，见 [ADR-0062](./ADR-0062-deadcode-final-settlement.md)。

> 2026-08-09 追记：重新评估触发条件——决策二"不做列表"两项均有明确复议条件
> （SurrealKV 双轨检索：压测证明现有 Registry+Selector 瓶颈后；多签审批：出现
> 真实多用户/多操作者场景后），前提不变则维持已驳回；决策五 M4/M5 压缩接入
> 若发现 Stage1"大 tool_result 优先卸载"的保守替代长期无法逼近真实 salience
> 排序效果，才重议引入更复杂的排序算法。
