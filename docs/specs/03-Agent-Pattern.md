# 03 AI Agent 层规范

> 适用于 `pkg/cognition/`（认知核心）和 `pkg/swarm/`（协同学习）的代码生成约束。

## AGENT-1 状态机持有控制流（HE-Rule-5 实例化）

所有涉及 LLM 调用的路径都必须是 Go 状态机的某个步骤，不存在独立的 `callLLMAndWait`。

FSM 11 态核心流转（S_IDLE 为空闲等待态，S_INTERRUPT 可从任意活跃态触发，详见 M04 §1）：

```
s_perceive → s_plan → s_validate → s_execute → s_reflect → s_complete
                ↓           ↓          ↓
            s_replan    s_rollback  s_failed

任意活跃态 ──(UserInterrupt / KillSwitch)──→ s_interrupt
```

- **System 1**（SurpriseIndex < 0.3）：零 LLM 调用，走缓存/规则路径
- **System 1.5**（0.3 ≤ SI < 0.6）：轻量 LLM，temperature 0
- **System 2**（SI ≥ 0.6）：重量推理，temperature > 0，Best-of-N

新建 Agent 行为路径时必须：定义状态转换 → 定义事件 → 注册 handler。禁止自由调用 LLM 后再判断。

## AGENT-2 Event-Driven 通信

Agent 之间不直接通信。所有跨 Agent 交互通过 Blackboard：

```
Agent A → EventTaskPosted → Blackboard → CAS Acquire → Agent B
```

- 禁止 Agent 之间共享内存、channel、直接函数调用
- 任务认领用 CAS（Compare-And-Swap），重试 3 次后放弃
- Lease TTL = 60s，Reaper 1s 扫描过期任务

参考 `pkg/swarm/orchestrator.go` 的 `ListenLoop` + `dispatchPendingTasks`。

## AGENT-3 Memory 访问分层

四层记忆（`pkg/cognition/memory/memory.go`）按层隔离：

| Layer | 写入源 | 读取范围 | 持久化 |
|-------|--------|----------|--------|
| Working | 当前 DAG 步骤 | 当前执行单元 | 否（上下文结束后清除） |
| Episodic | Agent 自动写 | 同 Agent 类型 | 是（events 表） |
| Semantic | Consolidation 产出 | 全局 | 是（semantic_memory 表） |
| Procedural | Skill 编译产出 | 全局 | 是（skills 表） |

写入必须带 TaintLevel。`MemoryEntry` 的 `TaintSource` 字段不可为空。

## AGENT-4 Skill 生命周期

Skill 三件套（`skills/builtin/SKILL.md + schema.json + wasm.wasm`）：

```
创作 → Logic Collapse（System 2 轨迹编译为 Wasm）→ 注册 → System 1 零推理执行
```

- Skill 三件套（SKILL.md + schema.json + impl.wasm）均在 `skills/builtin/` 目录；impl.wasm 经 `go:embed` 打入二进制，运行时由 `EmbedWasmLoader` 从 embed.FS 加载（非直接文件系统读取）；元数据注册信息（skill ID、版本、签名）写入 skills 表
- AI 生成的 Skill 必须经过 M6 四层 S_VALIDATE

## AGENT-5 SurpriseIndex 路由决策

权威定义见 `[SurpriseIndex]` `00-Global-Dictionary §3` + M09 §2.0：

```
SI = embeddingCosineDistance × 0.4
   + toolSequenceDivergence  × 0.35
   + MEMFMatchScore          × 0.25
```

权重由 M9 DynamicDifficultyCalibrator 按 task_type 自适应调整。

- SI < 0.3：走 System 1 缓存路径，零 LLM
- SI > 0.85：跳过 Auto-Curriculum 生成（系统过载）
- SI 读取：通过 `SetSurpriseIndexProvider(fn func() float64)` 注入，nil 时返回 0.5

参考 `pkg/swarm/self_improve/engine.go:196` `currentSurpriseIndex()`。

## AGENT-6 TrustTier 准入约束（ADR-0016）

所有 Skill/Plugin 注册必须显式设置 `TrustTier`，禁止为零或默认推断。五级定义：

| TrustTier | 数值 | 来源 | 最大沙箱 | 审批 |
|-----------|------|------|---------|------|
| TrustSystem | 4 | Polaris 内置 | Sbx-L2/L3 | auto |
| TrustOfficial | 3 | MCP 官方白名单 | Sbx-L2 | auto |
| TrustCommunity | 2 | cosign 签名 | Sbx-L1 | prompt |
| TrustLocal | 1 | HMAC 本地签名 | Sbx-L1 | prompt |
| TrustUntrusted | 0 | 无签名 | **REJECT** | 禁止注册 |

编码强制项：
- AI 生成的 Skill 注册一律走 `TrustLocal`，经 M6 四层 S_VALIDATE 通过后可升 `TrustCommunity`
- 禁止将 `TrustSystem` 直接赋予非内置技能（硬编码抦截）
- Cedar 策略可基于 `trust_tier` 做 `permit/forbid` 决策；`TrustUntrusted` 则 fail-closed 拒绝注册

## AGENT-7 CodeAct 使用边界（M07 §7.4）

CodeAct 是 Ad-hoc 一次性代码执行，不沉淀为 Skill。

**允许使用 CodeAct 的唱尔展条件（全部满足）：**
1. 任务需要 ≥3 个工具组合 + 中间计算（单工具调用走标准 tool_call）
2. 当前会话 `trust_level ≥ 3` 且 `approval_status == "approved"`
3. 代码字段 `[TaintLevel] ≤ Medium`
4. 环境支持 L3 microVM（**Tier-0 确定返回 `ErrTier0SandboxLimit`**，禁止降级执行）

**禁止：**
- 将 CodeAct 当作“快捷 Skill”高频重复调用——高频可复用模式进入 Logic Collapse / Auto-Curriculum
- Tier-0 下任何形式的 CodeAct（没有安全容器即无隔离边界）
- 跳过 PolicyGate 直接构造 CodeAct 请求（fail-closed）
