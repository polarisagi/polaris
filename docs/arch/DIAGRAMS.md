# 关键流程时序图

> 跨模块关键流程的 Mermaid 时序图集合。AI 默认不加载;读单图按 `Read offset/limit` 定位。
> **§跳读**: 1:9 Taint / 2:69 KillSwitch / 3:134 EventLog / 4:195 Intent→Result / 5:225 ToolCall / 6:252 Multi-Agent黑板 / 7:277 Staging7阶段

---

## 1. Taint Tracking 全链路 {#taint-tracking}

> 覆盖: 外部输入 → SanitizeBySchema → workspace 写入 → RAG 检索 → Agent 上下文注入
> 关键防卫点: M4 TaintGate Layer A.1 字符串字段 content constraint (format/pattern/enum/const)

```mermaid
sequenceDiagram
    participant User as 用户/外部输入
    participant M11 as M11 TaintTracker
    participant M7 as M7 Tool Sandbox
    participant WS as M2 Workspace
    participant M10 as M10 RAG Ingester
    participant M5 as M5 HybridRetriever
    participant M4 as M4 TaintGate
    participant Prompt as M4 ContextAssembler

    User->>M11: 外部数据 (TaintHigh)
    M11->>M11: 打标 TaintHigh
    Note over M11: [Taint-Prop] output = max(inputs)

    alt LLM 摘要路径
        M11->>M11: SanitizeBySummarization
        Note over M11: TaintHigh → TaintMedium (硬地板)
    end

    alt JSON Schema 校验路径
        M11->>M11: SanitizeBySchema
        Note over M11: 检查每个 string 字段的 format/pattern/enum/const
        alt 字段有内容约束 (format/pattern/enum/const)
            Note over M11: TaintMedium → TaintLow ✅
            M11->>M11: 审计 taint_schema_downgrade
        else 字段无内容约束 (裸 {"type":"string"})
            Note over M11: 拒绝降级 ❌ — 维持 TaintMedium
        end
    end

    M7->>WS: workspace_write (TaintLow)
    Note over WS: [Taint-Prop] 写入文件元数据

    M10->>WS: 读取文件
    WS-->>M10: 文档 + TaintLevel 标注
    M10->>M10: ConnectorScheduler 打 InitialTaintLevel
    Note over M10: LLM 摘要 TaintFloor = TaintMedium

    M5->>M5: HybridRetriever.Search
    Note over M5: RRF 融合 BM25 + Dense + Graph
    M5->>M4: ScoredFragment[] (含 TaintLevel)

    M4->>M4: TaintGate — Layer A 上下文传播
    Note over M4: 非系统源 TaintMedium+ → LLM 产出继承最高 TaintLevel
    M4->>M4: Layer A.1 — 工具调用结构化降级
    Note over M4: 仅 schema 有 content constraint 的字段允许降级

    M4->>Prompt: ContextAssembler.Append
    Prompt->>Prompt: zone==ZoneImmutable && TaintLevel > TaintLow → panic
    Prompt->>Prompt: TaintMedium → ZoneTaintedData (不可进 ZoneImmutable)
    Note over Prompt: Build 顺序: ZoneImmutable → ZoneMutableSkill → ZoneTaintedData
```

---

## 2. KillSwitch 触发与响应链路 {#killswitch}

> 覆盖: M3 TokenBurnRate 检测 → M11 KillSwitch 阶段变迁 → M4/M8/M13 响应
> 关键防卫点: M4 仅读取 KillSwitch 阶段（不独立触发），M11 是 FSM 唯一权威持有者

```mermaid
sequenceDiagram
    participant M3 as M3 BurnRateDetector
    participant M11 as M11 KillSwitch FSM
    participant M4 as M4 Agent Kernel
    participant M8 as M8 Orchestrator
    participant M1 as M1 Inference
    participant M13 as M13 Server

    Note over M3: CANONICAL SOURCE — EMA_5s + EMA_30s

    loop 每 1s
        M3->>M3: 计算 EMA_5s, EMA_30s
        M3->>M3: 更新 polaris_token_burn_rate Gauge
    end

    alt Stage 1 触发条件
        Note over M3: EMA_5s > baseline.P95 × 2.0
        M3->>M11: 推送 TokenBurnRate (CANONICAL SOURCE)
        M11->>M11: CheckAndAct() → KillThrottle
        Note over M11: 动作: 降级 Tier 1, max_steps=3, 禁止写
        M11->>M11: 更新 polaris_killswitch_stage = 1 (Throttle)
        M4->>M11: 读取 polaris_killswitch_stage
        Note over M4: 响应: maxSteps=3, 禁止 write_network
    end

    alt Stage 2 触发条件
        Note over M11: Stage 1 持续 > 10min OR 安全违规
        M11->>M11: CheckAndAct() → KillPause
        Note over M11: 动作: 停止所有新任务, 保留状态, 通知
        M11->>M11: 更新 polaris_killswitch_stage = 2 (Pause)
        M4->>M11: 读取 polaris_killswitch_stage
        Note over M4: 响应: 完成当前 DAGNode → 不启动新任务
        M8->>M11: 读取 polaris_killswitch_stage
        Note over M8: 响应: 拒绝 PostTask, 挂起新 Agent
    end

    alt Stage 3 触发条件
        Note over M11: Stage 2 15min 未审批 OR 致命违规 OR [TokenBurnRate] > 10x 持续 30s
        M3-->>M11: polaris_token_burn_stage3_triggered_total Counter 边沿
        M11->>M11: CheckAndAct() → KillFullStop
        M11->>M11: executeFullStop()
        Note over M11: 1. 写 .fullstop 文件
        Note over M11: 2. orchestrator.StopAll → Executing→Suspended
        Note over M11: 3. inferenceRuntime.CancelAll
        Note over M11: 4. AuditTrail.Record
        Note over M11: 5. 200ms 内停止所有 tool call
        M11->>M11: 更新 polaris_killswitch_stage = 3 (Fullstop)
        M4->>M4: 读取 stage=3 → 立即取消 LLM 调用 → S_ROLLBACK
        M8->>M8: 读取 stage=3 → 所有 Agent goroutine 退出
        M1->>M1: CancelAll → 中断全部流式推理
        M13->>M13: SealedMiddleware → 503 (仅 /healthz + /_admin/unseal)
    end

    Note over M4,M11: M4 不独立触发 KillSwitch 阶段变迁
    Note over M4,M11: M3 → M11 推送是触发 KillSwitch 阶段变迁的唯一路径
```

---

## 3. EventLog 写入路径与崩溃恢复 {#eventlog}

> 覆盖: Agent Emit → EventWriteBuffer → DatabaseWriter → SQLite COMMIT → Outbox → 崩溃回放
> 关键防卫点: EventWriteBuffer 为纯缓冲层，所有写入经 DatabaseWriter 单写者串行化

```mermaid
sequenceDiagram
    participant Agent as M4 Agent
    participant EWB as M2 EventWriteBuffer
    participant MB as M2 MutationBus
    participant DW as M2 DatabaseWriter
    participant SQLite as SQLite WAL
    participant Outbox as M2 OutboxWorker
    participant M5 as M5 Episodic
    participant M9 as M9 MEMF/Heuristics

    Note over Agent,EWB: 热路径 (<1ms Emit)

    Agent->>EWB: Emit(StateTransitionEvent)
    Note over EWB: ch <- ev (<1μs)
    Agent->>EWB: Emit(StateTransitionEvent)
    Agent->>EWB: Emit(StateTransitionEvent)

    Note over EWB: 100ms ticker OR batch >= 64

    EWB->>EWB: leaseChecker.Verify (租约二次校验)
    Note over EWB: 失效事件 → 丢弃 + WARN

    EWB->>MB: Submit(MutationIntent{Priority=PriorityFlush, Table="events"})
    Note over MB: EventWriteBuffer 不持有独立写路径

    MB->>DW: ch <- intent
    Note over DW: 单写者串行化

    DW->>DW: flushBatch()
    DW->>SQLite: BEGIN IMMEDIATE
    DW->>SQLite: INSERT INTO events (批量)
    DW->>SQLite: INSERT INTO 业务表 (同事务)
    DW->>SQLite: COMMIT
    Note over DW: CompositeMutationIntent 跨 events+业务表原子提交

    SQLite-->>DW: COMMIT 成功

    loop Outbox 异步投影
        Outbox->>SQLite: SELECT * FROM outbox WHERE status='pending'
        Outbox->>M5: 投影到 episodic_events (+ embedding)
        Outbox->>M9: 投影到 HeuristicsMemory / MEMF
        Outbox->>SQLite: UPDATE outbox SET status='done'
    end

    Note over Agent,Outbox: === 崩溃恢复路径 ===

    Note over Agent: 进程崩溃, EventLog 完好

    Note over Agent: 重启: wake(sessionId)
    Agent->>SQLite: getEvents(sessionId) — 从 EventLog 回放
    Note over Agent: isReplaying = true (禁止 EmitEvent/ToolCall/Outbox)
    Agent->>Agent: 回放至 last success → 追平
    Note over Agent: isReplaying = false → 继续执行
---

## 4. Intent→Result 端到端时序图 {#intent-to-result}

> 覆盖: CLI 输入 → Intent 判定 → Agent 规划 → 执行验证 → 结果输出
> 关键路径包: `pkg/edge/scheduler`, `pkg/cognition/kernel`, `pkg/swarm/blackboard`

```mermaid
sequenceDiagram
    participant CLI as M13 Interface
    participant M4 as M4 Agent Kernel
    participant M1 as M1 Inference
    participant M7 as M7 Action Sandbox
    
    CLI->>M4: 提交用户 Intent
    Note over M4: FSM 状态: S_IDLE → S_PERCEIVE
    M4->>M1: LLM 结构化解析意图
    M1-->>M4: M4_PerceiveOutput
    Note over M4: FSM 状态: S_PERCEIVE → S_PLAN
    M4->>M1: 规划 DAG 子任务
    M1-->>M4: M4_PlanOutput (DAG)
    Note over M4: FSM 状态: S_PLAN → S_VALIDATE
    M4->>M4: 确定性校验图结构
    Note over M4: FSM 状态: S_VALIDATE → S_EXECUTE
    loop DAG 节点并发
        M4->>M7: ExecuteTool(taintLevel)
        M7-->>M4: ToolResult
    end
    Note over M4: FSM 状态: S_EXECUTE → S_REFLECT
    M4->>CLI: 返回 Result
```

## 5. Tool Call 完整执行链 {#tool-call-chain}

> 覆盖: 工具意图提取 → Sandbox 权限拦截 → 执行 → 审计记录
> 关键路径包: `pkg/action/tool`, `pkg/substrate/policy`

```mermaid
sequenceDiagram
    participant M4 as M4 Executor
    participant M7 as M7 ToolRegistry
    participant M11 as M11 PolicyGate
    participant Sbx as M7 Sandbox (wazero)
    participant M3 as M3 OTel/Audit
    
    M4->>M7: ExecuteTool(req, taintLevel)
    M7->>M11: IsAuthorized(req)
    alt 策略拒绝 (Cedar Forbid)
        M11-->>M7: Deny (ErrForbidden)
        M7-->>M4: TaintViolation
    else 策略放行
        M11-->>M7: Allow + CapabilityToken
        M7->>Sbx: Inject Token & Execute
        Sbx-->>M7: Sandbox Result
        M7->>M3: 记录 AuditLog
        M7-->>M4: ToolResult(Taint Propagation)
    end
```

## 6. Multi-Agent 黑板协调 {#multi-agent-blackboard}

> 覆盖: CAS 原子认领, 任务分配, Supervisor Tree
> 关键路径包: `pkg/swarm/orchestrator`

```mermaid
sequenceDiagram
    participant M8_Sup as M8 Supervisor
    participant BB as M8 Blackboard
    participant WorkerA as Agent A
    participant WorkerB as Agent B
    
    M8_Sup->>BB: PostTask(schema_event)
    WorkerA->>BB: Scan Pending Tasks
    WorkerA->>BB: CAS Claim (Agent_A)
    alt CAS 失败
        BB-->>WorkerA: Conflict
    else CAS 成功
        BB-->>WorkerA: Acquired + LeaseTTL
        WorkerA->>WorkerA: Execute Task
        WorkerA->>BB: Heartbeat (RenewLease)
        WorkerA->>BB: Mark Done (Result_Event)
    end
```

## 7. Staging 7 阶段流转 {#staging-7-stages}

> 覆盖: 自进化代码/技能从生成到全面上线的门控体系
> 关键路径包: `pkg/swarm/eval_runner`, `pkg/substrate/policy`

```mermaid
sequenceDiagram
    participant M9 as M9 Optimizer
    participant M12 as M12 Eval Harness
    participant M11 as M11 RolloutGate
    participant Prod as 生产流量
    
    M9->>M12: Stage 1: Candidate Emit
    M12->>M12: Stage 2: Static Analysis
    M12->>M12: Stage 3: Offline Eval (黄金集)
    M12->>M11: Stage 4: Canary Rollout (1%)
    M11->>Prod: 流量接入
    Prod-->>M11: OTel 指标监控
    M11->>M11: Stage 5: BurnRate/Surprise 校验
    alt 校验劣化
        M11->>M9: 熔断回滚
    else 校验通过
        M11->>M11: Stage 6: Gradual (10%→50%)
        M11->>M11: Stage 7: Full Promotion
    end
```

---

**END OF DIAGRAMS.md**
