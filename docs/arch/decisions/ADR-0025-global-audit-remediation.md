# ADR-0025: 全局架构审查缺陷修复综合档案（R21 / Phase 0 / Gemini 缺口）

**状态**: 已接受 (Accepted)
**日期**: 2026-06-14

## 背景

多子智能体全局审查（2026-06）对照 M01–M13 与 00-Global-Dictionary 比对代码，提出 18 项缺陷。经逐项代码核验：17 项属实，1 项（M05 上下文组装"全部缺失"）失实。本 ADR 收敛核验结论与修复方向（SSoT）。实施细节已内联于各修复提交；`docs/upgrade/` 目录下的升级指南（upgrade-01 ~ upgrade-05）记录了相关专项整改。

## 决策

按严重度分三档修复，对齐既有架构不变量，不引入新抽象。逐项根因→修复对应关系（细节见提交记录，不在此重复展开）：

**P0 安全/数据完整性**：① SurrealDB FFI 写函数静默丢弃错误伪装成功 → 引入 `SURREAL_ERR_QUERY` 真实回传错误码（HE-Rule-2 / M02 §10）。② `handleUpdateMCPServer` 未鉴权即改写 MCP 执行命令 → 补 PolicyGate，鉴权与落库分离为 `Manager.Authorize`（M13-bis / ADR-0016）。③ Wasmtime `WARM_POOL` 复用 `Store` 致内存无界累积 → 改为每次执行新建 Store 并 drop，仅复用 Engine/Module（M07 §4.3 / Tier-0 底线）。

**P1 逻辑/竞态**：④ `spawn_planner` 挂起型节点误触发 `TriggerExecuteDone` → 引入 `ToolResult.Suspended`（M04 / inv_global_08）。⑤ `AssignSandboxTier` 沙箱等级误判 → 严格对齐 M07 §4.2 规则 1–4。⑥ Provider 恢复缺 `ResumeFromSuspended` → 补齐，挂起任务可恢复（state.yaml §552）。⑦ `reap` 全局锁内做 DB 查询致优先级反转 → 缩小锁范围至 `cancels` map（M08 §1.7）。⑧ `extension_instances` 双写 → 统一为每条安装路径仅一次 INSERT（ADR-0019 Layer-1 SSoT）。⑨ `ExecuteTool` 未签发 JIT Token、不可逆操作未 DryRun → 接入 `NewJITToken` + DryRun 预飞（M07 §5.4/§6 / inv_M7_05）。

**P2 健壮性/可观测**：⑩ Cedar 评估 goroutine 未传 ctx 致死锁泄漏 → ctx 贯穿 evaluate/FFI。⑪ Cedar 降级无告警 → 补 `slog.Warn` + `polaris_cedar_degraded_total`。⑫ Linux 内存探针低估可用内存 → 优先解析 `/proc/meminfo MemAvailable`。⑬ `legacyMetricsHandler` 指标缺失 → 对照 M03 清单补齐。⑭ `Reaper.Phase2` 终态 GC 从未调用致 `tasks` 表膨胀 → 改用 `reaper.Run`。⑮ WASM 输出 `\0` 被静默替换损坏二进制 → 显式报错或改长度前缀 ABI。

## 核验更正

**M05 上下文三区组装"全部缺失"——失实**。三个组装模块均已实现且有测试；但复核发现更严重的真实缺陷：三个 builder 把 episodic/semantic/whisper 等外部检索数据直接拼进 `system` 角色且无 Spotlighting 围栏（Prompt Injection 面），且存在与 `kernel.PromptBuilder`（M11 §3 唯一合法组装器）功能重叠的死代码。已修复：三 builder 统一到 `PromptBuilder` + 污点围栏，删除重复死代码。当前实现现状见 `docs/arch/M05-Memory-System.md`。

## 后果

- **正面**：消除两处 P0 数据/安全底线缺口（静默丢写、越权改命令）与一处 Tier-0 内存破坏（Store 池化）；恢复挂起任务可恢复性与状态机正确性；补齐安全引擎与内存探针的可观测性。
- **负面**：A9（JIT Token + DryRun）改动面较大，拆分两提交；A13 指标补齐先交核心子集，余项入 backlog。

## Phase 0 缺口（原 ADR-0028）

代码核验发现 4 项影响系统正确性的 P0 缺陷，均属"已有基础设施但未接线"类型。

- **BUG-A (Scheduler 防抖)**：`scanAndDispatch` 接入 `BackgroundPermit(taskPriority)`，负载过高时累积 `MissedExecutions`。
- **BUG-B (FSM SafeGo)**：用 `concurrent.SafeGo` 替换裸 goroutine，防止 panic 导致 Blackboard 死锁。
- **BUG-C (Cedar evaluate SafeGo)**：用 `concurrent.SafeGo` 包装 Cedar 评估 goroutine，消除泄漏。
- **BUG-D (SurpriseCalculator 接入)**：`Agent` 新增 `surpriseCalc SurpriseReader` 接口注入，主路径调用 `SubmitToolSeq` / `CurrentSurprise` 替代 `ComputeBasic`。

## Gemini 执行缺口（原 ADR-0027）

`04-remediation-final-complete.md` 修复方案执行后的 4 项"假完成"缺口：

- **BUG-1 (LAM PolicyGate)**：注入真实 `LAMPolicyChecker` (方法 `CheckPolicy`) 到 Agent，在 HITL 审批前插入 Cedar 策略预检，实现 deny-by-default。
- **BUG-2 (ResourceBudget)**：将四处的零值 `&budget.ResourceBudget{}` 替换为 `budget.NewResourceBudget(tbr, guard, gate)`，确保三维门控全量生效。
- **BUG-3 (m9-bb-bridge SafeGo)**：Blackboard→M9 事件桥改为 `concurrent.SafeGo`（功能已确认随全量 SafeGo 迁移完成）。
- **BUG-4 (GetEntity taint_level)**：修复 SELECT 缺 `taint_level` 列绑定的问题，确保 XR-16 读过滤生效。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-14 | 初稿，Accepted |
| 2026-06-17 | 移除对不存在文件 GEMINI_PATCH_R21/R22.md 的引用；实施详情已内联于提交记录，专项整改见 docs/upgrade/upgrade-01~05 |
| 2026-06-25 | Gemini 执行 04-remediation-final-complete.md 后发现 4 项实现缺口（R1/R2/R3/R4 假完成），已修复并归档至原 ADR-0027 |
| 2026-07-04 | 精简：15 项修复的详细实现步骤收敛为一行根因→修复摘要，决策依据与后果保持不变，详细实现见提交记录 |
| 2026-07-09 | 综合合并：将原 ADR-0027（Gemini 缺口）和 ADR-0028（Phase 0 缺口）的内容并入本 ADR，形成统一的修复档案 |
