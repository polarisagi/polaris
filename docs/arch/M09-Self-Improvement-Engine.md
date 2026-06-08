# 模块 9: Self-Improvement Engine

> 三环嵌套进化（经验→技能→架构），全无梯度主线（[Tier-0-Limit] 8GB 完整运行）。梯度训练仅 local_only 可选。
> Go 编排 + Eval 驱动 + Consolidation + 全部自进化逻辑。 [HE-Rule-4] [HE-Rule-5] [HE-Rule-6]
> **§跳读**: 0-bis:6 职责 / 0-ter:20 不变量速查 / 1:33 五路线(CANONICAL) / 2:91 三环嵌套 / 3:253 五级演化+审批 / 4:284 条件梯度 / 6:308 369(SOFT)降级 / 7:328 依赖
## 0-bis. 职责边界

| M9 **是** | M9 **不是** |
|-----------|-------------|
| 无梯度自进化（Reflexion/Distillation/Curriculum/Fallacy/Personalization） | 模型训练（QLoRA 仅 HT1+ 可选路径，不参与架构主线） |
| PromptOptimizer 三融合算法（GEPA/MemAPO/ContraPrompt） | Prompt 的最终使用决策（那是 M4 PromptFn） |
| MEMF 失败记忆池 + HeuristicsMemory 成功启发式库 | 记忆的物理存储（那是 M2） |
| Auto-Curriculum 边缘任务自动生成 | 任务执行（那是 M4 + M8） |
| SurpriseIndex 完整版计算（三组件含 MEMF，异步推送至 M3） | SurpriseIndex 基础版计算（那是 M3） |
| Staging 7 阶段 candidate_emit（Stage 1） | Staging 其余阶段的门控执行（Stage 2-7 由 M11/M12 负责） |
| ProgressiveRollout 阶段推进决策 | 流量分发执行（那是 M13 TrafficSplitter） |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M9_01 | 所有自进化候选必经 staging 7 阶段——禁止 M9 worker 直写生产表 | M9 → M2 Outbox staging 路径审计 |
| inv_M9_02 | 梯度训练仅 local_only + HT1+ 可选——不参与架构主线 | M3 HardwareProbe 门控 |
| inv_M9_03 | PromptOptimizer 输出经独立 LLM-as-Judge（不同 Provider 模型）安全审查 | M9 §1.1 输出安全流水线 |
| inv_M9_04 | Auto-Curriculum 在 Ephemeral Namespace 内执行——禁止 write_network/privileged | M9 §2.2 Ephemeral Namespace |
| inv_M9_05 | L4 不可变内核受 CI pre-receive hook 三重保护——白名单外修改自动拒绝 | CI `immutable_kernel_check` |
| inv_M9_06 | Frequency + 语义方差双门控——单一信号不足以触发 Logic Collapse | M9 §2.0 + M6 §2.2 |

---

## 1. 五条无梯度自改进路线（CANONICAL SOURCE）

```
(a) Eval 驱动 Prompt 优化: Eval Harness 回归 → 识别退化 → 自动调整 system prompt [HE-Rule-4]
(b) 经验重放与反思: 失败轨迹 → LLM 反思 → Episodic Memory
(c) 技能蒸馏 (Logic Collapse): 成功轨迹 → LLM 编译为 Wasm 技能 → Skill Library [Wasm-Sandbox]
(d) 检索式个性化: 用户偏好 + 纠错历史 → UserProfile.InteractionSummary
(e) Activation Steering: Control vector 推理时注入，仅 local_only + Tier 1+
```

**local_only 模式下系统能力降级表**:
| 能力 | 是否可用 | 替代方案 |
|------|---------|---------|
| (a) Eval 驱动 Prompt 优化 | 可用 | — |
| (b) 经验重放与反思 | 可用 | — |
| (c) Logic Collapse | Tier 0 禁用 / Tier 1+ 本地 TinyGo 编译 | 仅加载预编译技能 |
| (d) 检索式个性化 | 可用 | — |
| (e) Activation Steering | Tier 0 禁用 | 无替代 |
| Auto-Curriculum | 受限（仅 bash_restricted 工具白名单） | — |
| **语义向量检索** | **Tier 0 永久降级** | 本地 BGE-small (384-dim) 与 SurrealDB-Core 索引 (4096-dim) 维度不兼容，M05 §12.1 Phase 1 触发 BM25 + GraphTraverse 接管。远程重嵌不可用 → 无法进入 Phase 2-3 恢复。Tier 1+ 可选加载 BGE-large (1024-dim) + 线性投影到 4096-dim，须经 M12 Eval 验证 Recall@10 ≥ 0.85 |
用户首次进入 local_only 模式时主动提示能力降级影响（含语义检索降级告警）。

### 1.1 PromptOptimizer — GEPA + MemAPO + ContraPrompt 三融合

实现见 `pkg/swarm/prompt_optimizer.go`。

三种优化策略融合: GEPA（遗传-Pareto 搜索，种群 8 × 5 代前沿探索）、MemAPO（双记忆跨任务复用——从成功历史中学习通用模式）、ContraPrompt（对比轨迹——从成功和失败的差异中提取改进信号）。每种策略独立产出候选 prompt，经 Pareto 前沿去重后合并。

**触发条件**（OR 逻辑）: (1) tasks ≤ 100 且每 20 次触发（冷启动专属，满 100 次后关闭）；(2) 当前 score 低于 baseline 的 95%（检测退化）；(3) 距上次优化超过 50 个任务（定期巡检）。早停: 连续 2 代前沿无新非支配解时终止本轮优化。Token 预算软上限 30K/周期，空闲期放宽至 2 倍。

**输出安全流水线** (写入 M5 ZoneMutableSkill 前):
1. M11 Taint Gate `SanitizeBySchema` + `SanitizeByDeterministicTransform` → 阳性 → 丢弃 + 审计事件 `prompt_opt_taint_rejected`
2. SIC 检测（间接指令注入）→ 阳性 → 丢弃 + 审计事件
3. 独立 LLM-as-Judge 安全审查（与 PromptOptimizer 使用不同 Provider 模型）: 前 5 次输出全部审查；累计 5 次通过后改为 20% 概率抽查，优先抽查语义距离 >2σ 的异常输出。Judge 返回 unsafe → 丢弃 + [ESCALATE]
4. Ed25519 签名 → M5 ZoneMutableSkill
详细 Taint Gate 流程见 M5 §2.1 "M9 → ZoneMutableSkill Taint Gate（双层）"。

### 1.2 BackgroundTaskScheduler

实现见 `pkg/swarm/curriculum.go:BackgroundTaskScheduler`。

后台任务按五级优先级调度: 0=Consolidation（记忆压缩，最高优先）、1=LogicCollapse（技能自动生成）、2=Reflection（失败反思）、3=AutoCurriculum（课程自动生成）、4=PromptOptimizer（最低优先）。

**空闲门控**: L0/L1 级任务不受限制。L2+ 级任务需要同时满足: CPU 占用率低于 `spec/state.yaml §m9_self_improve.worker_cpu_pct_user_active` 持续超过 `worker_heartbeat_seconds`、空闲内存 >1.5GB、交流电源供电、无全屏应用——四项条件全部满足才允许入队。运行中的 L2+ 任务在条件破坏时被挂起（同步宽限期）。电池供电时仅允许 L0 级任务。`/config background_tasks off` 暂停所有后台任务。

**状态与游标持久化 (HE-Rule-6)**: 
Background Worker 消费 EventLog 产生自进化样本时，必须维护自身的消费游标。每个 Worker 在处理完一个 Batch 之后，必须通过 CAS (Compare-And-Swap) 将 `last_processed_event_seq` 同步写入 M2 的 `sys_config` 表中，以确保原子推进。配合下游阶段写入操作的幂等性设计（通过 `idempotency_key` 约束），严格保障崩溃恢复时的 Exactly-Once 处理语义。严禁 Worker 依赖纯内存队列或在重启后漏消费/重复消费进度。

### 1.3 Activation Steering（local_only + Tier 1+ 专属）

通过 Control Vector 在本地模型推理时注入 hidden_state 偏移量，实现推理时的行为操控。同层（默认 layer_id=15）的多个 Control Vector 按 Weight×Strength 加权求和后叠加到 hidden_state。CV hash 变更时驱逐当前 Session 的 KV Cache（仅影响当前 Session）。成功率低于 0.1 的 Control Vector 自动停用。

**Tier 限制与限制理由**: Tier 0 不可用（→ ErrTierInsufficient，回退路线 a-d）。Activation Steering 需要本地模型才能注入 hidden_state——远程 API 不暴露 hidden_state 接口。Tier 1+（16GB+ RAM）才能运行足够大的本地模型（Qwen3-8B+）产生有意义的 steering 效果。Tier 0 跑得动的 3B 级模型注入效果差——非内存约束，而是本地模型能力的限制。

用户命令: `/steer list|set <label> <weight>|deactivate|delete|calibrate-layer <task_type>`

---

## 2. 三环嵌套进化架构

### 2.0 Surprise_Index [SurpriseIndex] — 权威计算实现

SurpriseIndex 的完整三组件计算逻辑归属 M9，因 MEMFMatchScore 依赖本模块的 FallacyMemoryPool。M3 内置基础计算器（两组件简化版，详见 M3 §4.0）作为 staleness>60s 时的回退。M4 优先消费 M9 推送的完整版（M4 §5 RouteReasoning 步骤 0）。

**架构决议说明**（2026-05-08，对齐 ROADMAP §4.4）:
ROADMAP §4.4 决议了"三层机会主义架构"（Layer A logprob / Layer B Markov + embedding / Layer C BurnRate），与本节下方的 Phase 1/2/3 描述并非两套独立方案，而是同一演进路线的不同视角：

| ROADMAP 层次 | 对应本节 Phase | 说明 |
|------------|------------|------|
| Layer B (Markov + embedding，主信号) | Phase 1 → Phase 2 演进 | Phase 1 = embedding + 编辑距离 + MEMF；Phase 2 = embedding + 马尔可夫条件概率 + MEMF |
| Layer A (logprob，实验 side-channel) | Phase 3 | 机会主义收集，≥6 个月数据后评估是否升为主信号 |
| Layer C (BurnRate，兜底) | **不进入 SurpriseIndex 计算式** | TokenBurnRate 是 M11 KillSwitch 的独立熔断信号（M11 §4.3 BurnRateFuse），与 SurpriseIndex 语义不重叠。ROADMAP Layer C 指"当 SurpriseIndex 不可用时的系统兜底策略"，而非 SurpriseIndex 的分量 |
| MEMF 分量 | Layer B 的第三分量（本文档扩展） | ROADMAP 未列出 MEMF，但 MEMF 是 Layer B 的内部信号增强项，不影响层次划分 |

三阶段定义描述 Layer B 的演进路径（主信号），Layer A 和 Layer C 独立运作，不修改三阶段阈值语义。

**Layer B 演进路径**（toolSequenceDivergence 实现升级）:

**当前实现（Tier 0 默认基线）**: 三组件 `embeddingCosineDistance + toolSequenceDivergence(Levenshtein) + MEMFMatchScore`。冷启动 (<10条) → 0.5。TotalTrajectories<100 时轨迹追加 `bootstrapping=true` 标签防噪音污染。延迟 100-300ms。

**演进方向（500+ 成功轨迹后）**: toolSequenceDivergence 升级为马尔可夫链状态转移矩阵条件概率，热路径 O(1) 查表，公式形态不变。构建矩阵时排除 `bootstrapping=true` 数据。前置条件见 ROADMAP §3.1。

**Layer A（实验性，不修改主计算式）**: per-token logprob 机会主义旁路收集，依赖上游 Provider 暴露 logprob（DeepSeek V4/Claude 全系不暴露；2027+ 可能）。≥6个月数据后评估是否叠加。

路由阈值: si < low → System 1 | low ≤ si < high → System 1.5 | si ≥ high → System 2。默认 low=0.30 / high=0.60，由 `M9SelfImproveThresholds.SurpriseRouteLowThreshold/HighThreshold`（`configs/threshold-examples/m9_self_improve.toml`）覆盖，DynamicDifficultyCalibrator 可在此基础上动态调整。

SurpriseIndex 类型和 Compute/Route 实现见 `pkg/swarm/surprise.go`。SurpriseCalculator 持有 context.CancelFunc，调用 `Close()` 可优雅停止 4 个 worker goroutine（防模块重启泄漏）。

**BoundedWorkQueue + LoadShedder**:
```
队列: cap=256. 消费者: 固定 4 worker (不扩缩——饱和 API QPS)

LoadShedder:
  1. 队列满 → 每 3 个丢弃 1 个 (33%)
  2. 使用率 > 90% 持续 30s → 50%
  3. 使用率 < 50% → 0%
  4. 丢弃 → M3 polaris_surprise_embedding_dropped Counter++

优先级: 深度 > 64 → background/auto_curriculum 直接丢弃
```

**异步错误处理**:
```
单次: goroutine 内重试 1 次 (1s 退避), context.WithTimeout(5s)
连续: 最近 3 次 ≥ 2 次失败 → 保持上次值 + WARN + M3 polaris_surprise_async_failures++
持续: > 10min 失败 → safe default 0.5 + ALERT
恢复: 首次成功即退出 safe default
```

计算结果异步推送至 M3 `polaris_surprise_index` Gauge，M4 通过读取该 Gauge 进行路由。M3 staleness 监控独立检测值更新间隔。

### 2.1 内环: 经验积累（实时/小时级）

每次 Agent 任务完成后自动执行:

```
任务完成 → [EventLog] → 成功: HeuristicsMemory + Logic Collapse → Wasm 技能 [Wasm-Sandbox]
                       → 失败: Reflexion 反思 → MEMF + [Storage-SurrealDB-Core] + 发布 EventHeuristicGenerated (注入 PromptOptimizer 规避规则)
                       → Consolidation Check → Semantic Memory (M5 L2)
                       → Preference Learner (冷路径异步) → UserProfile
```

**MEMF** (FallacyMemoryPool) / **HeuristicsMemory** 类型和反馈校准/剪枝逻辑见 `pkg/swarm/memf.go`。

**Critic / Veto** (后台协程并行):
- Critic: [TokenBurnRate]>2× → 干预; [SurpriseIndex] 持续升高 → 标记偏离; MEMF 高危匹配 → 剪枝; 安全红线 → Veto
- Veto (LLM 不可覆盖): 安全红线 → 中断+回滚; [TokenBurnRate]>4× → 降级; 连续3次 Veto → [KillSwitch] Stage1
- Failure Clusterer: 同类失败>3 → L2+ 自修改队列

**DynamicDifficultyCalibrator**: 实现见 `pkg/swarm/prompt_optimizer.go` (DynamicDifficultyCalibrator)。基于最近 50 条 DifficultySample (TaskType/SurpriseIndex/Success) 动态调整 SurpriseIndex 阈值。冷启动（<20 条历史）使用静态 canonical 阈值 [0.3, 0.6]。successRate<0.5 时每步下调 0.05（下限 0.1），successRate>0.7 时每步上调 0.05（**上限 0.85**），targetSuccessRate=0.6。上限 0.85 与 §2.2 Auto-Curriculum MaxCurriculumDifficulty 硬上限对齐——当 DynamicDifficultyCalibrator 将阈值调至 [0.3, 0.85] 区间时，Auto-Curriculum 步骤 6 的 `目标难度 = currentHigh` 不会触发硬上限拦截，消除"高成功率场景下课程任务静默禁止生成"的状态不一致。

### 2.2 中环: 技能演化（日/周级）

技能成功率统计触发 + 定期后台任务:

```
成功率 < 30% 且使用 > 10 → 触发演化 (排除 UncontrollableFailure)
成功率 > 90% 且使用 > 50 → 金牌技能 (高优先级缓存)
连续 3 次 ControllableFailure → deprecated
SKILL.md → 收集轨迹 → LLM 编译 Wasm → System1 执行
已是 Wasm → 边缘案例 → TinyGo+wazero 验证 → version++
```

**Auto-Curriculum Generator** 类型和生成流程见 `pkg/swarm/curriculum.go`。

生成流程:
1. IdleDetector.IsIdle() → 非空闲跳过（OS 可用内存 < 512MB 或 Goroutine 数 ≥ 200 视为繁忙）
2. SkillGapAnalysis: >90% 成功率 → 更难变体; 50-90% → 相似难度不同场景; <30% → 跳过
3. **MaxCurriculumDifficulty 硬上限**: SurpriseIndex ≤ 0.85（超过不生成），防持续生成不可完成任务
4. 同一 SourceSkill 连续 3 次生成的课程任务全部失败 → 临时冻结该技能的课程生成 60 分钟
5. Curriculum 任务总成本占 BackgroundTaskScheduler 预算的硬上限 20%
6. LLM 生成 (每技能 ≤3, 总 ≤10/周期), 目标难度 = currentHigh
7. 生成后安全审查（四阶段）:
   (a) M11 Taint Gate 扫描任务描述中的注入载荷
   (b) 若含 shell/bash 命令 → 危险模式黑名单拒绝（同原列表）
   (c) M11 SIC Cleaning 检测间接 prompt injection（"忽略指令"/"override"/"你是"/"现在你是" 等角色劫持模式 + 语义越界检测）。检测到 injection → 丢弃任务 + 写 curriculum_injection_blocked 审计事件
   (d) 独立 LLM-as-Judge 安全审查（使用与课程生成不同的 Provider 模型，避免同模型自我审查盲区）: 判定任务描述是否包含隐藏的恶意指令或社会工程诱导。Judge 返回 unsafe → 丢弃任务 + [ESCALATE]。前 10 次全部审查，之后置信度 >0.95 可改为 20% 抽查
   审查未通过 → 写 curriculum_hazard_log + 丢弃该课程任务
8. [Sandbox-L2] 影子执行验证 → [Blackboard].PostTask(priority=0)
9. 课程任务失败不进 MEMF（防止污染失败记忆池）; 成功率连续 < 20% → 标记 SourceSkill 需中环演化

**Ephemeral Namespace**: FS → `os.TempDir()/auto_curriculum/{task_id}/` (任务后清理); NotesStore → 临时 SQLite; Semantic Graph → 临时图层 (≥80%成功率+Critic审核后合并); Episodic Memory → session_type='auto_curriculum'; 剥夺 write_network/read_network/privileged; 工具白名单: file_read, file_write, bash_restricted (字符集白名单+Wasm受限挂载); 网络需求 → localhost mock HTTP

### 2.3 外环: 架构演化（周/月级）

```
Gate 1: Eval Harness 离线回归 — 全部黄金用例 + Welch's t-test p<0.05 → 发布 EventEvalCompleted
Gate 1: Shadow (1% 流量) — `SubmitCandidate` 立即进入此阶段。`RecordEvalScore` 异步补充 Eval 结果：passRate < baseline×0.95 触发自动 Rollback；passRate ≥ baseline×1.05 将 status 激活为 running。
Gate 2: Shadow Execution (3-7天) — `ConfirmShadow` 确认影子指标正常后推进至 Canary 5%。candidate 版本执行真实任务但输出不面用户，安全护栏禁止 write_network + privileged。
Gate 3: Canary Rollout（阶梯: 5%→25%→50%→100%，每步驻留 24h）
  硬停止: error>baseline×1.2 | P95 latency>baseline×1.4 | 安全违规>0 | SurpriseIndex 退化 → autoRollback
Gate 4: Full Rollout, 旧版本保留 7 天 rollback target
```

实现见 `pkg/swarm/rollout.go` (ProgressiveRollout) 和 `pkg/swarm/rollout_store.go` (SQLiteRolloutStore)。持久化状态表 `rollout_states` 新增 `eval_score`（Eval 得分）与 `shadow_ok`（影子确认标志）列。Gate 转换由三个方法驱动：`SubmitCandidate`（Gate 0→1，自动进入 Shadow）、`RecordEvalScore`（Eval 结果写入，不达标自动 Rollback）、`ConfirmShadow`（Gate 1→2，影子观测通过）；`AdvanceGate` 仅处理 Gate 2+ 的 Canary 推进，不覆盖 Gate 0/1 专属路径。硬停止条件全局适用于所有 Gate。`StagingPipeline` 接口保持稳定，M13 TrafficSplitter 按 `canary_percent` 分发。

### 2.4 Cross-Module Co-Evolution [Module-Topology] [Blackboard]

防止单模块进化导致其他模块退化:

| 触发 | Regression | Compensation | 自动化 |
|------|-----------|-------------|--------|
| M5 Consolidation | M6 技能库 precondition 沙箱重跑 | 失败 → needs_adaptation + Logic Collapse | L2 半自动 |
| M6 Logic Collapse 新技能 | M4 System1 阈值 ±0.05 影子执行 50次 | 退化 >3% → 回退阈值 + L0 重标定 | L0 全自动 |
| M10 重嵌入 | M5 HybridRetriever Recall@10 对比 | 退化 >5% → RRF 权重调整 (Vector -0.05) | L0 全自动 |
| M1 Provider 升级 | M4 DAG template × M12 Golden Eval | P0 失败 → 阻止; P1 → PromptOptimizer 冷启动 | L1 全自动 |
| M7 新 MCP tool | M4/M6 同名工具引用向后兼容检查 | 不兼容 → needs_adaptation + 更新 tool name | L2 半自动 |

实现见 `pkg/swarm/prompt_optimizer.go` (CoEvolutionCoordinator, CoEvolutionEvent, CoEvSubscriber)。CoEvolutionCoordinator 维护 module→subscriber 映射，监听跨模块变更事件 (CoEvolutionEvent: SourceModule/ChangeType/ChangeLevel)。变更经 [Blackboard].Publish(EventCoEvolution) → 联合回归 → 退化按级别补偿 (L0 调参 / L1 prompt / L2+ LogicCollapse) → [EventLog]。

---

## 3. 合成评测数据生成（EvalGenerator）

实现见 `pkg/governance/synthetic/generator.go`（`EvalGenerator`）。由 M9 BackgroundTaskScheduler 离线批量触发，**禁止在 RunSuite 热路径中调用**。输出 `SyntheticCase` 经调用方适配器转为 `EvalCase(SourceSynthetic)` 注入 M12 Training Set。

### RAGAS Evolution 三阶段流水线

输入 M10 知识库 chunks，输出 SyntheticCase（Severity 硬上限 P2，needs_human_audit=true）。

- **Stage 1 — Simple 生成**：对每个 chunk 用 deepseek-chat 生成一个事实性 QA 对（Type: factual/easy，Temperature=0.7 保多样性）。
- **Stage 2 — Evolution 难度演化**：按 chunk 索引 %3 分流：multi_hop 多步推理（medium）/ counterfactual 反事实推理（hard）/ 保持 factual/easy 不演化。
- **Stage 3 — Groundedness 验证**：Judge LLM 过滤，答案无法从 chunk 找到依据的丢弃；通过者设 ContextBound=true。
- **n-gram 去重**：3-gram SHA-256 指纹去除同义复述，保留语义不同变体。

### EvalGenerator 配置

| 字段 | 默认值 | 说明 |
|------|--------|------|
| Enabled | false | 须显式启用 |
| TargetRatio | 0.05 | 每 100 chunks 生成 5 条（向上取整，最少 1 条） |
| provider | protocol.Provider | LLM 批量生成入口，必须注入 |

### SyntheticCase 结构

类型定义见 `pkg/governance/synthetic/generator.go`（字段：ID/Question/GroundTruth/ChunkID/Type/Difficulty/ContextBound）。调用方需实现 `SyntheticCase → EvalCase` 适配（设 `Source=SourceSynthetic, Severity=P2, needs_human_audit=true`），M12 §5 DataSplitter 将 `SourceSynthetic` 路由至 Training Set。
流程: L2+ 变更 → [Blackboard].Publish(EventCoEvolution) → 联合回归 → 退化按级别补偿 (L0 调参 / L1 prompt / L2+ LogicCollapse) → [EventLog]

---

## 3. 五级演化层次 + 审批门控

| 级别 | 范围 | 自动化 | 门控 | 回滚 |
|------|------|--------|------|------|
| **L0** | 配置调整 (路由权重/超时/模型选择/[SurpriseIndex]分位) | 全自动 | telemetry 监控 | 即时 |
| **L1** | Prompt/Heuristic/system prompt/路由判据 | 全自动 | Eval Harness | 即时 |
| **L2** | 新技能生成 (Skill Library 新条目) | 半自动 | 沙箱 + Eval + cosign | 即时 |
| **L3** | 策略修改 / LoRA 适配器 | 需审批 | Shadow + Canary + 多签 | 分钟级 |
| **L4** | 源码/架构修改 (Go/Rust) | 严格审批 | 形式化验证 + Red Team + 多签 | git revert |

**L4 不可变内核** — `0400` + CI merge-block + pre-receive hook 三重保护:

| 不可变包 | 保护理由 |
|---------|---------|
| `pkg/substrate/policy/` | [Cedar-Gate] + [Taint-Prop] 安全决策 |
| `pkg/substrate/policy/audit/` | 审计日志不可否认性 |
| `pkg/substrate/observability/` | [HE-Rule-1] 可观测管线 |
| `pkg/cognition/kernel/` | [KillSwitch] 熔断链路 |
| `pkg/action/sandbox/` | [Sandbox-L2] 边界实施 |
| `internal/config/immutable_constants.go` | 编译期不可变常量 |
| `internal/protocol/interfaces.go` | 安全接口契约 (SafeDialer/Cedar-Gate/TaintLevel/Blackboard) |
| `internal/protocol/schema/` | EventLog DDL + Outbox DDL，M11 hash chain 依赖其完整性 |

**L4 白名单 (Default-Deny)**: `pkg/swarm/**`, `pkg/cognition/skill/**`, `pkg/cognition/memory/**`, `pkg/edge/**`。CI/CD 配置 + 构建入口 + go.mod 等全部禁止 L4 修改。

**CI 强制**: L4 PR → process-external CI pipeline (独立进程，非进程内 eval) → `immutable_kernel_check` 扫描 → 命中白名单外 → auto reject (2 安全审批者 override)。CI 配置自身受 pre-receive hook 保护。L4 自进化不得在运行进程内执行 Holdout Set 评估——必须通过独立 CI runner。

实现见 `pkg/swarm/prompt_optimizer.go` (AutoConfigOptimizer)。L0 级自动配置优化: OptimizeRouteWeights() 基于 7 天 ProviderStats 调整路由权重（cps<Avg×0.8→weight+0.1，cps>Avg×1.5→weight-0.1）；CalibrateSurpriseThresholds() 基于 30 天 System 1 命中率取 P10/P50/P90 替代静态 [0.3, 0.7]。

---

## 4. 条件梯度训练（硬件门控 → AutoConfig 自动化）

QLoRA/PRM/ActivationSteering 的 Tier 门控由 `FeatureGate` 自动化：`FeatureQLoRA`(≥Tier1+≥4GB→1-3B; ≥Tier2+≥8GB→7B)、`FeaturePRMTraining`(≥Tier2+≥8GB)、`FeatureActivationSteer`(≥Tier1+≥1.5GB+LocalInference 已启用)，启动时自动判定，调用方使用 `GlobalFeatureGate().IsEnabled(FeatureQLoRA)`，详见 M03 §5。

梯度训练仅 local_only 可选，不参与架构主线。[Phase0-Bootstrapping] 时 HardwareCapabilityProbe 判定。

| 路径 | 最低内存 | GPU | 框架 |
|------|---------|-----|------|
| QLoRA 4-bit | 16GB RAM + 8GB VRAM | RTX 3060+ / M2+ | unsloth/mlx-lm |
| QLoRA 8-bit | 32GB RAM + 24GB VRAM | RTX 4090 / M3 Max+ | unsloth/bitsandbytes |
| Full FT | 64GB+ RAM + 48GB+ VRAM | A6000 / M4 Ultra | axolotl/torchtune |

**梯度训练硬件门控**: 根据 GPU VRAM 和系统 RAM 自动判定可行路径——VRAM≥24GB + RAM≥32GB → QLoRA 8-bit；VRAM≥8GB + RAM≥16GB → QLoRA 4-bit；Apple Silicon unified memory≥16GB → MLX LoRA；不满足则 PathNone（回退无梯度路线）。

**Go 侧实现边界**: M9/M1 只负责触发和对接，训练本身由外部进程承载：
- `QLoRAAdapter`（`pkg/substrate/inference/adapter_training.go`）→ HTTP POST `localhost:8000/v1/train/qlora`，发送 `{Prompt, Completion}` 样本集
- `PRMAdapter` → HTTP POST `localhost:8001/v1/train/prm`，同格式
- `GET /v1/export/trajectories` → 导出 ShareGPT/OpenAI JSONL，供外部 SFT/DPO 工具消费
- DPO Chosen/Rejected 数据配对、GRPO Rollout Group、DARE/TIES 适配器合并均为**前置条件未满足的研究方向**，待本地训练服务稳定后再实现（见 ROADMAP §3）

任何梯度训练步骤失败即回退无梯度路线（路线 a-e），不阻塞主线。

---

## 6. 降级与失败模式（5 问全覆盖）

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| Worker goroutine 崩溃 (Reflexion/Distillation/Curriculum/Fallacy) | suture OneForOne 重启 + backoff（权威源 `spec/state.yaml §m9_self_improve.worker_restart_backoff_initial_ms` / `worker_restart_backoff_max_seconds`） | 5 次上限 → Escalate Root Supervisor |
| PromptOptimizer 候选生成为空 | 跳过本周期 → 延长触发间隔 | 下次周期正常触发恢复 |
| PromptVersionStore.OnActivate 回调 nil 或 panic | 激活写库成功，回调失败不回滚；仅热更新路径失效 | Server 重启后从 DB 热恢复 |
| MEMF 池检索超时 (>50ms) | 跳过剪枝直接放行 | 池大小削减后恢复 |
| Auto-Curriculum 任务失败 | 标记课程任务 failed→Ephemeral Namespace 绑定，不影响核心功能 | — |
| Staging Stage 失败 | candidate → rejected/dead_letter → audit | 下一代候选重新进入 |
| CoEvolutionCoordinator 订阅者 OnChange 失败 | L0 调参 / L1 prompt / L2 LogicCollapse 退化补偿 | 手动介入 |
| GraphRAG LLM 调用日预算超限 | 跳过剩余 graph_build_task + 设置 next_retry_at = 次日 00:00 UTC（保持 pending，M2 Outbox Worker 跳过未来重试时间） | 次日自动恢复 |

与 OSMemoryGuard 协同: L1 预警 → 挂起 Auto-Curriculum + 暂停后台 worker 池 / L2 紧急 → 挂起 Consolidation + Reflexion / L3 临界 → 全部自进化活动暂停。


## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m9_self_improve`。

## 7. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M4 Agent Kernel | SurpriseIndex consumer → System 1/1.5/2 路由 | M4 §5 |
| M5 Memory | ZoneMutableSkill Taint Gate（双层）、PersonaRefiner、UserProfile 写入权 | M5 §2.1 |
| M6 Skill Library | Logic Collapse 触发门控（成功次数 >=50 + 语义方差 + Eval Gate）| M6 §2.2 |
| M8 Orchestrator | Auto-Curriculum PostTask（priority=3）→ Blackboard CAS 认领 | M8 §1 |
| M11 Policy Safety | PromptOptimizer 输出安全流水线（Taint Gate → SIC → 独立 LLM-as-Judge）| M11 §2 |
| M12 Eval Harness | Eval 门控（Training/Validation/Holdout 三层分区、PromptOptimizer 早停依据），通过 EventEvalCompleted 驱动外环 Rollout | M12 §5 |
| M13 Interface | TrafficSplitter 执行分发、ResourceGovernor 空闲门控；`PromptVersionStore.OnActivate` 回调热更新 `ImmutableCore`（`task_type='general'`） | M13 §2.5 + M5 §11.0 |
| 全局字典 | HE-Rule-4 数据驱动迭代、MEMF/HeuristicsMemory/GEPA 定义 | 00-Global-Dictionary §2, §9-bis |
| 事件总线协议 | EventHeuristicGenerated（内环规避规则注入）、EventEvalCompleted（外环评测结果驱动 Activate） | internal/protocol/event.go |
| DDL | sys_prompt_versions（Prompt 版本化）、skill_variant_pool（工具描述符变体池）| internal/protocol/schema/010_self_improve.sql |
| 时序图 | KillSwitch 触发链（自进化在 KillSwitch 各阶段的响应）| DIAGRAMS.md#killswitch |

---

## 8. 实现状态与 2026 研究对照

### 当前实现状态

三环架构（Reflexion/Curriculum/Rollout）+ PromptOptimizer + MEMF + SurpriseIndex 均已实现（`self_improve/engine.go`、`reflexion.go`、`curriculum.go`、`rollout.go`、`memf.go`、`surprise.go`）。**✅ 核心路径已完成**。

### 引入计划

| 研究 | 来源 | 核心机制 | 引入点 | 优先级 |
|------|------|---------|-------|-------|
| **D-MEM 多巴胺门控** | arXiv:2603.14597, 2026 | 将 SurpriseIndex 输出接入 M5 Consolidation 的情节→语义晋升决策，仅高 surprise 事件触发巩固写入；与现有 SurpriseIndex 已有协同基础 | `pkg/swarm/surprise.go` → `pkg/cognition/consolidation.go` §9 | P1 |
