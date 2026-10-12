# 03 AI Agent 层规范

> 适用于 `internal/agent/`、`internal/memory/`、`internal/swarm/` 的代码生成约束。

## AGENT-1 状态机持有控制流（HE-5 实例化）

所有涉及 LLM 调用的路径都必须是 Go 状态机的某个步骤，不存在独立的 `callLLMAndWait`。

FSM 共 14 态（权威：`pkg/types/enums_agent.go`；转移表见 `docs/arch/M04-Agent-Kernel.md §1`，注册于 `internal/agent/fsm/transitions*.go`）：主链 `s_perceive → s_plan → s_validate → s_execute → s_reflect → s_respond → s_complete`；旁路 `s_replan` / `s_rollback` / `s_failed` / `s_await_agent`（跨节点委派挂起）；`s_idle` 空闲等待、`s_suspended` 空闲挂起；`s_interrupt` 可由 UserInterrupt / KillSwitch 从任意活跃态触发。`s_respond` 是唯一面向用户的自由文本态（ADR-0098）。

- **System 1**（SI < 0.3）：零 LLM，走缓存/规则路径
- **System 1.5**（0.3 ≤ SI < 0.6）：轻量 LLM，temperature 0
- **System 2**（SI ≥ 0.6）：重量推理，temperature > 0，Best-of-N

新建 Agent 行为路径必须：定义状态转换 → 定义事件 → 注册 handler。禁止自由调用 LLM 后再判断。

## AGENT-2 Event-Driven 通信

Agent 之间不直接通信，一切跨 Agent 交互经 Blackboard：`Agent A → EventTaskPosted → Blackboard → CAS Acquire → Agent B`。

- 禁止 Agent 间共享内存、channel、直接函数调用
- 任务认领用 CAS，重试 3 次后放弃；Lease TTL = 60s，Reaper 1s 扫描过期任务（阈值权威源 `spec/state.yaml`）
- 实现与参考：`internal/execute/orchestrator/`（`sqlite_blackboard.go`、Worker `ListenLoop`）

## AGENT-3 Memory 访问分层

四层记忆（`internal/memory/memory.go`）按层隔离：

| Layer | 写入源 | 读取范围 | 持久化 |
|-------|--------|----------|--------|
| Working | 当前 DAG 步骤 | 当前执行单元 | 否 |
| Episodic | Agent 自动写 | 同 Agent 类型 | 是（events 表） |
| Semantic | Consolidation 产出 | 全局 | 是（semantic_memory 表） |
| Procedural | Skill 编译产出 | 全局 | 是（skills 表） |

写入必带 TaintLevel，`MemoryEntry.TaintSource` 不可为空。

## AGENT-4 Skill 生命周期

创作 → Logic Collapse（System 2 轨迹蒸馏为 Python 技能，ADR-0008）→ 注册 → System 1 零推理执行。

- Skill 三件套（SKILL.md + schema.json + src/skill.py）由扩展市场统一拉取，主仓库不内置；运行于 ContainerSandbox L3（`def execute(input: dict) -> dict:`）；元数据（ID、版本、签名）写入 skills 表
- AI 生成的 Skill 必须经 M6 四层 S_VALIDATE

## AGENT-5 SurpriseIndex 路由决策

权威定义 `00-Global-Dictionary §3` + `M09 §2.0`：`SI = embeddingCosineDistance×0.4 + toolSequenceDivergence×0.35 + MEMFMatchScore×0.25`，权重由 M9 DynamicDifficultyCalibrator 按 task_type 自适应。

- SI < 0.3 走 System 1 缓存路径；SI > 0.85 跳过 Auto-Curriculum 生成（系统过载）
- SI 经 `SetSurpriseIndexProvider(fn func() float64)` 注入，nil 时返回 0.5（参考 `internal/learning/engine.go`）

## AGENT-6 TrustTier 准入约束（ADR-0103 决策九，原 ADR-0016）

所有 Skill/Plugin 注册必须显式设置 `TrustTier`，禁止零值或默认推断：

| TrustTier | 值 | 来源 | 最大沙箱 | 审批 |
|-----------|----|------|---------|------|
| TrustSystem | 4 | Polaris 内置 | Sbx-L2/L3 | auto |
| TrustOfficial | 3 | MCP 官方白名单 | Sbx-L2 | auto |
| TrustCommunity | 2 | cosign 签名 | Sbx-L1 | prompt |
| TrustLocal | 1 | HMAC 本地签名 | Sbx-L1 | prompt |
| TrustUntrusted | 0 | 无签名 | **REJECT** | 禁止注册 |

- AI 生成的 Skill 一律注册为 `TrustLocal`，过 M6 四层 S_VALIDATE 后可升 `TrustCommunity`
- 禁止把 `TrustSystem` 赋予非内置技能（硬编码拦截）
- Cedar 可基于 `trust_tier` 做 permit/forbid；`TrustUntrusted` fail-closed 拒绝注册

## AGENT-7 CodeAct 使用边界（M07 §7.4）

CodeAct 是 Ad-hoc 一次性代码执行，不沉淀为 Skill。**仅当全部满足才可使用**：

1. 任务需 ≥3 个工具组合 + 中间计算（单工具走标准 tool_call）
2. 会话 `trust_level ≥ 3` 且 `approval_status == "approved"`
3. 代码字段 `[TaintLevel] ≤ Medium`
4. 沙箱可用：L3 microVM；Tier-0 降级为 `SandboxNativeOS`（bwrap/Seatbelt，物理隔离），`CapPrivileged` 在 Tier-0 仍返回 `ErrTier0SandboxLimit`（ADR-0008 决策一）

**禁止**：把 CodeAct 当"快捷 Skill"高频重复调用（高频可复用模式进 Logic Collapse / Auto-Curriculum）；Tier-0 下降级为无隔离执行；跳过 PolicyGate 直接构造 CodeAct 请求（fail-closed）。
