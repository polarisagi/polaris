# ADR-0025: 全局架构审查缺陷修复（R21）

**状态**: 已接受 (Accepted)
**日期**: 2026-06-14

## 背景

多子智能体全局审查（2026-06）对照 M01–M13 与 00-Global-Dictionary 比对代码，提出 18 项缺陷。经逐项代码核验：17 项属实，1 项（M05 上下文组装"全部缺失"）失实。本 ADR 收敛核验结论与修复方向（SSoT）。实施细节已内联于各修复提交；`docs/upgrade/` 目录下的升级指南（upgrade-01 ~ upgrade-05）记录了相关专项整改。

## 决策

按严重度分三档修复，对齐既有架构不变量，不引入新抽象。

**P0 安全/数据完整性**
1. SurrealDB FFI（`surreal_store.rs`）8 个写函数静默丢弃错误并恒返 `SURREAL_OK`、`surreal_kv_get` 以 `.unwrap_or(None)` 把 DB 错误伪装为 NOT_FOUND → 引入 `SURREAL_ERR_QUERY`，真实回传错误码（违反 HE-Rule-2 / M02 §10）。
2. `handleUpdateMCPServer`（`mcp_servers.go`）未鉴权即改写 MCP 执行命令 → 补 PolicyGate（对齐 create 路径 / M13-bis / ADR-0016）。鉴权与落库分离为 `Manager.Authorize`。
3. Wasmtime `WARM_POOL` 复用 `Store`（Arena 无单实例回收）致内存无界累积 → 改为每次执行新建 Store 并 drop，仅复用 Engine/Module（M07 §4.3 / Tier-0 底线）。

**P1 逻辑/竞态**
4. `spawn_planner` 同时推 `TriggerInterruptReceived` 与 `TriggerExecuteDone`，后者在 `S_INTERRUPT` 无转移 → 引入 `ToolResult.Suspended`，挂起型节点不再触发 ExecuteDone（M04 / inv_global_08）。
5. `AssignSandboxTier` 将 LLMGenerated/MCP/A2A/WriteNetwork 误判为 L3（与 M07 §4.2 文档相反，应为 L2），且缺规则 4 非 Linux Tier0 降级 → 严格对齐 §4.2 规则 1–4。
6. `NewProviderRecoveryHandler(nil,...)` + `SQLiteBlackboard` 缺 `ResumeFromSuspended` → 注入 blackboard 并实现该方法，挂起任务可恢复（state.yaml §552）。
7. `reap` 在全局锁内做 DB 查询/行迭代致优先级反转 → 缩小锁范围至 `cancels` map 操作（M08 §1.7）。
8. `InstallExtension` 与 gateway handler 双写 `extension_instances` → 统一为每条安装路径仅一次 INSERT（ADR-0019 Layer-1 SSoT）。
9. `ExecuteTool` 仅依赖全局 PolicyGate，未签发 JIT Token、不可逆操作未 DryRun → 接入 `NewJITToken`（TTL/MaxCalls）+ §5.4 DryRun 预飞（M07 §5.4/§6 / inv_M7_05）。

**P2 健壮性/可观测**
10. `gate.go IsAuthorized` 起 goroutine 未传 ctx，Cedar FFI 死锁则永久泄漏 → ctx 贯穿 evaluate/FFI。
11. Cedar 引擎降级 Go 兜底时无告警无指标 → 补 `slog.Warn` + `polaris_cedar_degraded_total`。
12. `probeOSMemory`（Linux）用 `free+buffers` 忽略 page cache，低估可用内存致过早降级 → 优先解析 `/proc/meminfo MemAvailable`（复用 `internal/sysmgr/sysinfo`）。
13. `legacyMetricsHandler` 仅暴露 7 指标 → 对照 M03 清单补齐核心业务指标。
14. `main.go` 自写 Phase1-only ticker，`Reaper.Phase2`（终态 GC）从不调用致 `tasks` 表膨胀 → 改用 `reaper.Run`。
15. `write_err` 对 WASM 输出中的 `\0` 静默替换为 `?` 损坏二进制 → 显式报错或改长度前缀 ABI。

## 核验更正

**M05 上下文三区组装"全部缺失"——失实**。`internal/agent/context/memory_context.go`、`epoch.go`、`consolidation.go` 均已实现且有测试。但深入复核发现**更严重的真实缺陷**：`memory_context.go` 的三个 builder 把 episodic/semantic/whisper 等外部检索数据直接拼进 `system` 角色且**无 Spotlighting 围栏**（Prompt Injection 面），而 `ContextAssembler` 系与 `kernel.PromptBuilder`（M11 §3 唯一合法组装器）功能重叠的死代码（零非测试调用方）。**修复方案（R22 已修复）**：三 builder 统一到 `PromptBuilder` + 污点围栏、Epoch 上主路径、删除重复 `ContextAssembler`/`epoch.go`。

## 后果

- **正面**：消除两处 P0 数据/安全底线缺口（静默丢写、越权改命令）与一处 Tier-0 内存破坏（Store 池化）；恢复挂起任务可恢复性与状态机正确性；补齐安全引擎与内存探针的可观测性。
- **负面**：A9（JIT Token + DryRun）改动面较大，拆分两提交；A13 指标补齐先交核心子集，余项入 backlog。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-14 | 初稿，Accepted |
| 2026-06-17 | 移除对不存在文件 GEMINI_PATCH_R21/R22.md 的引用；实施详情已内联于提交记录，专项整改见 docs/upgrade/upgrade-01~05 |
| 2026-06-25 | Gemini 执行 04-remediation-final-complete.md 后发现 4 项实现缺口（R1/R2/R3/R4 假完成），已修复并归档至 ADR-0027 |
