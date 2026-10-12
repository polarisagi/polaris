# 模块 9: Self-Improvement Engine

> 三环嵌套进化（经验→技能→架构），全无梯度主线（[Tier-0-Limit] 8GB 完整运行）。梯度训练仅 local_only 可选。
> Go 编排 + Eval 驱动 + Consolidation + 全部自进化逻辑。 [HE-Rule-4] [HE-Rule-5] [HE-Rule-6]
<!-- §跳读: 0-bis:6 职责 / 0-ter:20 不变量速查 / 1:34 五路线(CANONICAL) / 2:78 三环嵌套 / 3-bis:177 EvalGenerator / 3:194 五级演化+审批 / 4:224 条件梯度 / 6:245 305 (SOFT)降级 / 6-bis:261 321 已知Bug修复记录 / 7:277 336 依赖 / 8:295 354 实现状态+2026研究对照 -->
## 0-bis. 职责边界

| M9 **是** | M9 **不是** |
|-----------|-------------|
| 无梯度自进化（Reflexion/Distillation/Curriculum/Fallacy/Personalization） | 模型训练（QLoRA（Quantized Low-Rank Adaptation，量化低秩适应） 仅 HT1+ 可选路径，不参与架构主线） |
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

- **启动**：`m9Engine.Run(ctx)` 经 Supervisor Tree（`sv.AddWorker("m9-engine", ...)`）启动，享受 OneForOne 崩溃重启；禁止裸 goroutine。
- **CC-2 ResourceBudget 接线**（ADR-0025 BUG-2）：`m9Engine.WithBackgroundGate(...)` 必须传 `budget.NewResourceBudget(sb.TBR, memGuard, featGate)`，禁止零值 `&budget.ResourceBudget{}`（退化为单维认知压力门控，TBR P95 与内存降级维度失效）。`Blackboard→M9` 事件桥须走 `concurrent.SafeGo(ctx, "m9-bb-bridge", ...)`（XR-14，ADR-0025 BUG-3）。

## 1. 五条无梯度自改进路线（CANONICAL SOURCE）

| 路线 | 机制 |
|------|------|
| (a) Eval 驱动 Prompt 优化 | Eval Harness 回归 → 识别退化 → 自动调整 system prompt [HE-Rule-4] |
| (b) 经验重放与反思 | 失败轨迹 → LLM 反思 → Episodic Memory；成功轨迹经 `RecordSuccess` 写 HeuristicsMemory |
| (c) Logic Collapse | 成功轨迹 → LLM 生成 Python 脚本 → ContainerSandbox 执行 → Skill Library（≥8GB 且 L3 可用，并发=1；<8GB VPS 禁用，ADR-0008） |
| (d) 检索式个性化 | 用户偏好 + 纠错历史 → UserProfile.InteractionSummary |
| (e) Activation Steering | Control vector 推理时注入（仅 Tier 3+，local_only） |

**local_only 模式能力降级**：(a)(b)(d) 可用；(c) <8GB VPS / L3 不可用时禁用，仅加载预生成技能；(e) Tier 0/1/2 禁用，无替代；Auto-Curriculum 受限（仅 `bash_restricted` 工具白名单）；**语义向量检索**默认走进程内 ONNX 嵌入器（统一 512 维，ADR-0109），嵌入器不可用 → 降级 FTS（M05 §12.1）。用户首次进入 local_only 时主动提示降级影响。

### 1.1 PromptOptimizer — GEPA + MemAPO + ContraPrompt 三融合

实现 `internal/learning/`（PromptOptimizer）。GEPA（遗传-Pareto 搜索，种群 8 × 5 代）、MemAPO（双记忆跨任务复用）、ContraPrompt（成功/失败对比轨迹）各自产出候选 prompt，经 Pareto 前沿去重后合并。

**触发条件**（OR）: (1) tasks ≤ 100 且每 20 次触发（冷启动专属）；(2) 当前 score 低于 baseline 的 95%；(3) 距上次优化超过 50 个任务。早停: 连续 2 代前沿无新非支配解。Token 预算软上限 1000K/周期。

**输出安全流水线**（写入 M5 ZoneMutableSkill 前）:
1. **Taint Gate**: M11 `SanitizeBySchema` + `SanitizeByDeterministicTransform` → 阳性 → 丢弃 + 审计 `prompt_opt_taint_rejected`
2. **SIC 检测**: 间接指令注入 → 阳性 → 丢弃 + 审计
3. **独立 LLM-as-Judge**: 与 PromptOptimizer 用不同 Provider 模型。前 5 次全审；累计 5 次通过后 20% 抽查，优先语义距离 >2σ 的异常输出。unsafe → 丢弃 + `[ESCALATE]`
4. **签名**: Ed25519 → M5 ZoneMutableSkill

Taint Gate 详见 M5 §2.1 "M9 → ZoneMutableSkill Taint Gate（双层）"。

### 1.2 BackgroundTaskScheduler

实现 `internal/learning/`。五级优先级: 0=Consolidation（最高）、1=LogicCollapse、2=Reflection、3=AutoCurriculum、4=PromptOptimizer。

**空闲门控**: L0/L1 不受限。L2+ 需同时满足：CPU 占用低于 `spec/state.yaml §m9_self_improve.worker_cpu_pct_user_active` 持续超过 `worker_heartbeat_seconds`、空闲内存 >1.5GB、交流电源、无全屏应用；运行中任务条件破坏时挂起（同步宽限期）。电池供电仅允许 L0。`/config background_tasks off` 暂停所有后台任务。

**事件消费（channel-based，HE-Rule-6）**: `Engine`（`internal/learning/engine.go`）`select` 消费 `taskEvents`/`versionEvents`/`heuristicEvents`/`evalEvents` 四路只读 channel；统一游标表 `consumer_cursors`（consumer_id=`learning.task|version|heuristic|eval`，Go 侧白名单校验，ADR-0104 决策九）持久化四路游标，配合 idempotency_key 双重去重。

### 1.3 Activation Steering（local_only + Tier 1+ 专属）

Control Vector 在本地模型推理时注入 hidden_state 偏移。同层（默认 layer_id=15）多个 CV 按 Weight×Strength 加权求和；CV hash 变更时驱逐当前 Session 的 KV Cache；成功率 <0.1 自动停用。

**Tier 限制**: Tier 0/1/2 不可用（→ ErrTierInsufficient，回退路线 a-d）——远程 API 不暴露 hidden_state，只有 Tier 3（64GB+）能运行足够大的本地模型（Qwen3-32B+）产生有意义效果。

用户命令: `/steer list|set <label> <weight>|deactivate|delete|calibrate-layer <task_type>`

---

## 2. 三环嵌套进化架构

### 2.0 Surprise_Index [SurpriseIndex] — 权威计算实现

完整三组件计算归属 M9（MEMFMatchScore 依赖 FallacyMemoryPool）。M3 内置两组件简化版（M3 §4.0）作为 staleness>60s 回退；M4 优先消费 M9 推送的完整版（M4 §5 RouteReasoning 步骤 0）。

ROADMAP §4.4 "三层机会主义架构"与下方 Phase 是同一演进路线的不同视角：Layer B（Markov + embedding，主信号）= Phase 1（embedding + 编辑距离 + MEMF）→ Phase 2（embedding + 马尔可夫条件概率 + MEMF）；Layer A（logprob，实验 side-channel）= Phase 3，机会主义收集、≥6 个月数据后评估是否升为主信号（DeepSeek V4/Claude 全系不暴露 logprob），不修改主计算式；Layer C（BurnRate）**不进入计算式**——TokenBurnRate 是 M11 KillSwitch 独立熔断信号（M11 §4.3 BurnRateFuse）。

**当前实现（Tier 0 默认基线）**: `embeddingCosineDistance + toolSequenceDivergence + MEMFMatchScore`。冷启动 (<10条) → 0.5。TotalTrajectories<100 时轨迹追加 `bootstrapping=true` 标签防噪音污染。延迟 100-300ms。

**Layer B 马尔可夫矩阵**（`internal/learning/surprise/`，已实现，数据量门控激活）: `MarkovMatrix` 始终初始化并积累数据（启动时由 `session_trajectory` warm-start，ADR-0104 决策七）。`TotalTransitions() >= threshold`（`DefaultLayerBThreshold=1000`，`MinLayerBThreshold=500`，以代码为准）时 toolSequenceDivergence 自动切换为马尔可夫条件概率 O(1) 查表；构建矩阵时排除 `bootstrapping=true`；未达阈值前回落 Levenshtein 编辑距离。

路由阈值: si < low → System 1 | low ≤ si < high → System 1.5 | si ≥ high → System 2。默认 low=0.30 / high=0.60，由 `M9SelfImproveThresholds.SurpriseRouteLowThreshold/HighThreshold`（`configs/threshold-examples/m9_self_improve.toml`）覆盖，DynamicDifficultyCalibrator 可动态调整。

**BoundedWorkQueue + LoadShedder**：队列容量 256，固定 4 worker。队列满时丢弃 33%，使用率 >90% 持续 30s 升至 50%，<50% 恢复。深度 >64 的 background/auto_curriculum 请求直接丢弃。丢弃写 M3 `polaris_surprise_embedding_dropped` Counter。

**异步错误处理**：单次失败重试 1 次（1s 退避，5s 超时）；最近 3 次 ≥2 次失败 → 保持上次值 + WARN；持续 >10min 失败 → 降级 safe default 0.5 + ALERT；首次成功自动退出降级。

结果异步推送至 M3 `polaris_surprise_index` Gauge，M4 读取该 Gauge 路由；M3 staleness 监控独立检测更新间隔。

### 2.1 内环: 经验积累（实时/小时级）

每次 Agent 任务完成后：
- **成功路径**: `Engine.Run()` 调用 `RecordSuccess` 写 HeuristicsMemory + Logic Collapse 触发（ADR-0008）。
- **失败路径（Reflexion Closed-Loop）**: `ReflexionEngine` 反思 → heuristic 发送至 `heuristicCh` 被 Engine 消费（注入 PromptOptimizer）；`ReflectionWorker` 写 `reflection_memory`（M5 L2），下次任务 Perceive 阶段加载入上下文。
- **后续**: Consolidation Check → Semantic Memory（M5 L2）；冷路径异步 Preference Learner → UserProfile。

**MEMF**（FallacyMemoryPool）/ **HeuristicsMemory** 类型与反馈校准/剪枝见 `internal/learning/`。

**Critic / Veto**（后台协程并行）:
- **Critic**: `[TokenBurnRate] > 2×` → 干预；`[SurpriseIndex]` 持续升高 → 标记偏离；MEMF 高危匹配 → 剪枝；安全红线 → Veto
- **Veto**（LLM 不可覆盖）: 安全红线 → 中断+回滚；`[TokenBurnRate] > 4×` → 降级；连续3次 Veto → `[KillSwitch]` Stage1
- **Failure Clusterer**: 同类失败>3 → L2+ 自修改队列

**DynamicDifficultyCalibrator**（`internal/learning/`，已独立解耦）: 基于最近 50 条 DifficultySample（TaskType/SurpriseIndex/Success）动态调整阈值。冷启动（<20 条）用静态 [0.3, 0.6]。successRate<0.5 每步下调 0.05（下限 0.1），>0.7 每步上调 0.05（**上限 0.85**），targetSuccessRate=0.6。上限 0.85 与 §2.2 MaxCurriculumDifficulty 硬上限对齐，避免高成功率场景下课程任务被静默禁止生成。

### 2.2 中环: 技能演化（日/周级）

触发：成功率 <30% 且使用 >10 次（排除 UncontrollableFailure）；成功率 >90% 且使用 >50 次为金牌技能（高优先级缓存）；连续 3 次 ControllableFailure 标记 deprecated。SKILL.md 收集轨迹后 LLM 蒸馏为 TypeScript 脚本（System1 执行）；已是脚本技能经边缘案例触发验证后 version++。

**Auto-Curriculum Generator**（`internal/learning/curriculum/`）生成流程:
1. IdleDetector.IsIdle() → 非空闲跳过（OS 可用内存 < 512MB 或 Goroutine ≥ 200 视为繁忙）
2. SkillGapAnalysis: >90% 成功率 → 更难变体；50-90% → 相似难度不同场景；<30% → 跳过
3. **MaxCurriculumDifficulty 硬上限**: SurpriseIndex ≤ 0.85（超过不生成）
4. 同一 SourceSkill 连续 3 次生成的课程任务全部失败 → 冻结该技能课程生成 60 分钟
5. 总成本无硬上限，允许空闲时段全力生成
6. LLM 生成：每技能 `maxPerSkill=3`，每周期 `maxPerCycle=10`，目标难度 = `currentSurpriseIndex`
7. 安全审查（五阶段，ADR-0025 §G）:
   (0) **SQL 适应度预筛**（`SQLFitnessEvaluator`，`internal/learning/curriculum/fitness.go`，前置于其余检查）: 查 `events` 表 7 天窗口内该技能执行历史，`fitness = 成功率 × (1 - 平均预测误差)`；样本 ≥ 5 且 fitness < 0.5 → 直接拒绝（不调 LLM）；否则进入后续。nil-safe，未注入则跳过
   (a) M11 Taint Gate 扫描任务描述注入载荷
   (b) 含 shell/bash 命令 → 危险模式黑名单拒绝
   (c) M11 SIC Cleaning 检测间接 prompt injection（角色劫持 + 语义越界）→ 丢弃 + 审计 `curriculum_injection_blocked`
   (d) 独立 LLM-as-Judge（与课程生成不同 Provider）判定隐藏恶意指令/社会工程；unsafe → 丢弃 + [ESCALATE]。前 10 次全审，之后置信度 >0.95 可 20% 抽查
   未通过 → 写 `curriculum_hazard_log` + 丢弃
8. [Sandbox-L2] 影子执行验证 → [Blackboard].PostTask(priority=0)
9. 课程任务失败不进 MEMF（防污染）；成功率连续 < 20% → 标记 SourceSkill 需中环演化

**Ephemeral Namespace**: FS → `os.TempDir()/auto_curriculum/{task_id}/`（任务后清理）；NotesStore → 临时 SQLite；Semantic Graph → 临时图层（≥80% 成功率 + Critic 审核后合并）；Episodic Memory → session_type='auto_curriculum'；剥夺 write_network/read_network/privileged；工具白名单: str_replace_editor, glob, bash_restricted（字符集白名单 + Wasm 受限挂载）；网络需求 → localhost mock HTTP。

### 2.3 外环: 架构演化（周/月级）

**四级 Gate + Canary 阶梯发布门控**（`internal/learning/optimizer/rollout.go` `RolloutGate` + `rollout_store.go` `SQLiteRolloutStore`）：
1. **Gate 1 EvalRegression**: `SubmitCandidate` 后进入；Eval Harness 离线回归通过（`RecordEvalScore`）即满足，等待 Gate 2。
2. **Gate 2 ShadowExecution**: `ShadowExecutor`（`internal/eval/analysis/shadow_executor.go`，5 分钟周期，`cmd/polaris/boot_agent.go` 启动）从 `events` 表按游标增量抓取历史 `llm_call`，用候选配置重推理并与基线对比；通过率 ≥ `M12Eval.ShadowPassRateThreshold` → `ConfirmShadow` 推进 Gate 3，否则 `Rollback`。
3. **Gate 3 CanaryRollout**: `AdvanceGate` 按 canary 阶梯逐级推进灰度流量（阶梯与驻留时长见 `spec/state.yaml §m9_self_improve.canary_steps` / `canary_dwell_per_step_hours_*`；`rollout.go` 当前硬编码 1/5/25/50/100%）。
4. **Gate 4 FullRollout**: 100% 流量，`RolloutStatusCommitted`。

硬停止条件全局适用：error>baseline×1.2 / P95>baseline×1.4 / 安全违规 / SurpriseIndex 退化 → autoRollback（`ProgressiveRollout.CheckHardStop`）。

**候选生效的唯一入口**：`ConfirmShadow` 成功后经注入的 `promptActivator`（由 `*optimizer.PromptVersionStore` 实现）回调 `Activate`，读取 `SubmitCandidate` 时随 `AgentVersionSnapshot`（`TaskType`/`BaselineScore`）落盘的 `rollout_states.metadata`。`handleEvalCompleted` **不得**在 Eval 通过时直接 `Activate`（否则 Gate 2/3 对 GEPA 候选形同虚设，ADR-0025 §K）。

**单一状态源**：`m9Engine`（GEPA 候选）与 `LogicCollapseMonitor`（L2/L3/L4 候选）共用同一 `SQLiteRolloutStore`，由 `cmd/polaris/boot_agent.go` 构造并经 `AgentBundle.RolloutStore` 传给 `boot_server.go`。M13 TrafficSplitter 按 Gate 3 canary 百分比分发流量。

### 2.4 Cross-Module Co-Evolution [Module-Topology] [Blackboard]

防止单模块进化导致其他模块退化:

| 触发 | Regression | Compensation | 自动化 |
|------|-----------|-------------|--------|
| M5 Consolidation | M6 技能库 precondition 沙箱重跑 | 失败 → needs_adaptation + Logic Collapse | L2 半自动 |
| M6 Logic Collapse 新技能 | M4 System1 阈值 ±0.05 影子执行 50次 | 退化 >3% → 回退阈值 + L0 重标定 | L0 全自动 |
| M10 重嵌入 | M5 HybridRetriever Recall@10 对比 | 退化 >5% → RRF（Reciprocal Rank Fusion，倒数排名融合） 权重调整 (Vector -0.05) | L0 全自动 |
| M1 Provider 升级 | M4 DAG（Directed Acyclic Graph，有向无环图） template × M12 Golden Eval | P0 失败 → 阻止; P1 → PromptOptimizer 冷启动 | L1 全自动 |
| M7 新 MCP（Model Context Protocol，模型上下文协议） tool | M4/M6 同名工具引用向后兼容检查 | 不兼容 → needs_adaptation + 更新 tool name | L2 半自动 |

实现见 `internal/learning/`（CoEvolutionCoordinator）。CoEvolutionCoordinator 维护模块→订阅者映射，监听跨模块变更事件（含变更来源模块、变更类型、变更级别）。变更经 [Blackboard].Publish(EventCoEvolution) → 联合回归 → 退化按级别补偿 (L0 调参 / L1 prompt / L2+ LogicCollapse) → [EventLog]。

---

### 2.5 免疫系统：founding_anchor 漂移检测 + red_team 常态化对抗探测

两者由 `cmd/polaris/boot_agent.go` 启动为独立后台 goroutine，与三环并行——三环负责"变得更好"，本节负责"没有变坏"。

**founding_anchor 漂移检测**（`internal/eval/founding_anchor.go` + `internal/eval/harness`）：`FoundingAnchor` 是首次启动（或历史轨迹达 200 条会话上限）时对 Agent 行为的一次性指纹快照（`BehaviorFingerprint`，`ComputeFingerprint` 聚合近期 `TrajectoryTrace`）。此后每 24 小时取最近 50 条会话轨迹重算指纹，`CompareWithAnchor` 生成 `DriftReport`；`OverallDriftScore` 写入 `DriftMonitor`（`atomic.Value` 单例），`ShouldFreeze=true` 时触发 `TriggerCurriculum` 冻结课程生成。轨迹来源 `ChatRepository.ListSessions` + `TrajectoryRecorder.Record`，依赖 `sCtx.SessionID` 被正确赋值（§6-bis #3）。

**red_team 常态化对抗探测**（`internal/eval/red_team.go`，`RedTeamProtocol`）：4 条探针（L0 配置对抗 / L1 Prompt Injection / L2 恶意 Skill 描述 / L3 自引用 DAG 无限循环），经 `AgentPool.AcquireHeadless` 进程内拉起 Headless Agent 实测，按响应/错误是否含拒绝关键词（"blocked"/"refused"/"policy_violation"/"killswitch"）判定拦截。未通过标记 `SeverityP0` 并经 `InjectFindingsToHoldout` 写入 M12 Eval Holdout；通过标记 `SeverityP2`（仅记录）。与 [ADR-0091](./decisions/ADR-0091-gate-coverage-over-gate-results.md) 决策二（CI 层 PR 代码对抗审查 Action，原 ADR-0013/0014）是不同机制：前者审查提交的代码，本节审查运行中 Agent 的实际防御行为。

---

## 3-bis. 合成评测数据生成（EvalGenerator）

实现 `internal/learning/synthetic/synthetic_eval_gen.go`（`EvalGenerator`）。由 BackgroundTaskScheduler 离线批量触发，**禁止在 RunSuite 热路径调用**。输出 `SyntheticCase` 经调用方适配器转为 `EvalCase(SourceSynthetic)`（`Severity=P2, needs_human_audit=true`）注入 M12 Training Set（M12 §5 DataSplitter 路由）。

### RAGAS Evolution 三阶段流水线

输入 M10 chunks，Severity 硬上限 P2：
- **Stage 1 Simple**：每 chunk 用 deepseek-chat 生成一个事实性 QA 对（factual/easy，Temperature=0.7）
- **Stage 2 Evolution**：按 chunk 索引 %3 分流：multi_hop（medium）/ counterfactual（hard）/ 保持 factual/easy
- **Stage 3 Groundedness**：Judge LLM 过滤，答案无依据者丢弃；通过者 ContextBound=true
- **n-gram 去重**：3-gram SHA-256 指纹去同义复述

配置：`Enabled`（默认 false，须显式启用）、`TargetRatio`=0.05（每 100 chunks 生成 5 条，向上取整，最少 1）、`provider`（`protocol.Provider`，必须注入）。字段：ID/Question/GroundTruth/ChunkID/Type/Difficulty/ContextBound。


---

## 3. 五级演化层次 + 审批门控

| 级别 | 范围 | 自动化 | 门控 | 回滚 | 实现 |
|------|------|--------|------|------|------|
| **L0** | 配置调整（路由权重/超时/模型选择/[SurpriseIndex]分位） | 全自动 | telemetry 监控 | 即时 | `AutoConfigOptimizer` |
| **L1** | Prompt/Heuristic/system prompt/路由判据 | 全自动 | Eval Harness | 即时 | PromptOptimizer |
| **L2** | 新技能生成（Skill Library 新条目） | 半自动 | 沙箱 + Eval + HMAC-SHA256 签名 | 即时 | `LogicCollapseMonitor` + `StagingPipeline`（`logic_collapse_trigger.go`） |
| **L3** | 策略修改 / LoRA 适配器 | 需审批 | Shadow + Canary + 多签 | 分钟级 | `detectL3Trigger`：SurpriseIndex > 0.8 → HITL `l3_strategy_modify` → Staging |
| **L4** | 源码/架构修改（Go/Rust） | 严格审批 | 形式化验证 + Red Team + 多签 | git revert | `detectL4Trigger`：`l4TriggerCh` 管理员信号 → HITL `l4_multi_sig` → Staging |

**L4 不可变内核** — `0400` + CI merge-block + pre-receive hook 三重保护:

| 不可变包 | 保护理由 |
|---------|---------|
| `internal/security/` | [Cedar-Gate] + [Taint-Prop] 安全决策 |
| `internal/observability/` | [HE-Rule-1] 可观测管线 |
| `internal/agent/` | [KillSwitch] 熔断链路 |
| `internal/sandbox/` | [Sandbox-L2] 边界实施 |
| `internal/protocol/immutable_core.go` | 编译期不可变常量 |
| `internal/protocol/interfaces_*.go` | 安全接口契约（SafeDialer/Cedar-Gate/TaintLevel/Blackboard） |
| `internal/protocol/schema/` | EventLog DDL + Outbox DDL，M11 hash chain 依赖其完整性 |

**L4 白名单（Default-Deny）**: `internal/swarm/**`, `internal/extension/skill/**`, `internal/memory/**`, `internal/gateway/**`。CI/CD 配置 + 构建入口 + go.mod 等全部禁止 L4 修改。

**CI 强制**: L4 PR → process-external CI pipeline（独立进程，非进程内 eval）→ `immutable_kernel_check` 扫描 → 命中白名单外 → auto reject（2 安全审批者 override）。CI 配置自身受 pre-receive hook 保护。L4 自进化不得在运行进程内执行 Holdout Set 评估——必须经独立 CI runner。

L0 自动配置优化（`internal/learning/` AutoConfigOptimizer）：基于 7 天 ProviderStats 调路由权重（cps 低于均值 0.8× 上调，高于均值 1.5× 下调）；基于 30 天 System 1 命中率取 P10/P50/P90 替代静态阈值 [0.3, 0.7]。

---

## 4. 条件梯度训练（硬件门控 → AutoConfig 自动化）

Tier 门控由 `FeatureGate` 自动化：`FeatureQLoRA`(≥Tier1+≥4GB→1-3B; ≥Tier2+≥8GB→7B)、`FeaturePRMTraining`(≥Tier2+≥8GB)、`FeatureActivationSteer`(≥Tier1+≥1.5GB+LocalInference 已启用)，调用方用 `GlobalFeatureGate().IsEnabled(...)`（M03 §5）。梯度训练仅 local_only 可选，不参与架构主线。

| 路径 | 最低内存 | GPU | 框架 |
|------|---------|-----|------|
| QLoRA 4-bit | 16GB RAM + 8GB VRAM | RTX 3060+ / M2+ | unsloth/mlx-lm |
| QLoRA 8-bit | 32GB RAM + 24GB VRAM | RTX 4090 / M3 Max+ | unsloth/bitsandbytes |
| Full FT | 64GB+ RAM + 48GB+ VRAM | A6000 / M4 Ultra | axolotl/torchtune |

**硬件门控**: VRAM≥24GB + RAM≥32GB → QLoRA 8-bit；VRAM≥8GB + RAM≥16GB → QLoRA 4-bit；Apple Silicon unified memory≥16GB → MLX LoRA；不满足 → PathNone（回退无梯度路线）。

**Go 侧边界**: M9/M1 只负责触发和对接，训练由外部进程承载：
- `QLoRAAdapter`（`internal/llm/adapter/training.go`）: POST `localhost:8000/v1/train/qlora`，样本 `{Prompt, Completion}`；`PRMAdapter`: POST `localhost:8001/v1/train/prm`
- `GET /v1/export/trajectories`: 导出 ShareGPT/OpenAI JSONL，供外部 SFT/DPO 消费
- DPO 配对、GRPO Rollout Group、DARE/TIES 适配器合并为前置条件未满足的研究方向（ROADMAP §3）

任何梯度训练步骤失败即回退无梯度路线（a-e），不阻塞主线。

---

## 6. 降级与失败模式（5 问全覆盖）

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| Worker goroutine 崩溃 (Reflexion/Distillation/Curriculum/Fallacy) | suture OneForOne 重启 + backoff（`spec/state.yaml §m9_self_improve.worker_restart_backoff_initial_ms` / `worker_restart_backoff_max_seconds`） | 5 次上限 → Escalate Root Supervisor |
| PromptOptimizer 候选生成为空 | 跳过本周期 → 延长触发间隔 | 下次周期恢复 |
| PromptVersionStore.OnActivate 回调 nil 或 panic | 激活写库成功，回调失败不回滚；仅热更新路径失效 | Server 重启后从 DB 热恢复 |
| MEMF 池检索超时 (>50ms) | 跳过剪枝直接放行 | 池大小削减后恢复 |
| Auto-Curriculum 任务失败 | 标记 failed→Ephemeral Namespace 绑定，不影响核心功能 | — |
| Staging Stage 失败 | candidate → rejected/dead_letter → audit | 下一代候选重新进入 |
| CoEvolutionCoordinator 订阅者 OnChange 失败 | L0 调参 / L1 prompt / L2 LogicCollapse 退化补偿 | 手动介入 |
| GraphRAG LLM 调用日预算超限 | 跳过剩余 graph_build_task + `next_retry_at` = 次日 00:00 UTC（保持 pending，M2 Outbox Worker 跳过未来重试时间） | 次日自动恢复 |

与 OSMemoryGuard 协同: L1 预警 → 挂起 Auto-Curriculum + 暂停后台 worker 池 / L2 紧急 → 挂起 Consolidation + Reflexion / L3 临界 → 全部自进化活动暂停。


## 6-bis. 已知 Bug 修复记录

| # | 级别 | 位置 | 问题与修复 |
|---|---|---|---|
| 1 | P1 | DynamicDifficultyCalibrator | 历史 20–50 条时窗口负索引 panic、成功率分母错误 → 已修 |
| 2 | P0 | `internal/learning/engine_ops.go` handleEvalCompleted | Eval 通过即同步 `Activate`，Gate 2/3 形同虚设 → 改走 `ConfirmShadow` 唯一入口 + 统一状态源（§2.3） |
| 3 | P0 | `internal/agent/agent.go` NewAgent | `sCtx.SessionID` 未赋值，会话前缀塌缩为空串，`founding_anchor` 等失效 → `SessionID: id` |
| 4 | P0 | `internal/eval/red_team.go` runProbe | `Severity` 未赋值，Holdout 注入过滤恒真，发现被静默丢弃 → 按 `passed` 映射 P0/P2 |


---

## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m9_self_improve`。

## 7. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M4 Agent Kernel | SurpriseIndex consumer → System 1/1.5/2 路由 | M4 §5 |
| M5 Memory | ZoneMutableSkill Taint Gate（双层）、PersonaRefiner、UserProfile 写入权 | M5 §2.1 |
| M6 Skill Library | Logic Collapse 触发门控（成功次数 >=50 + 语义方差 + Eval Gate）| M6 §2.2 |
| M8 Orchestrator | Auto-Curriculum PostTask（priority=3）→ Blackboard CAS（Compare-And-Swap，比较并交换） 认领 | M8 §1 |
| M11 Policy Safety | PromptOptimizer 输出安全流水线（Taint Gate → SIC → 独立 LLM-as-Judge）| M11 §2 |
| M12 Eval Harness | Eval 门控（Training/Validation/Holdout 三层分区、PromptOptimizer 早停依据），通过 EventEvalCompleted 驱动外环 Rollout | M12 §5 |
| M13 Interface | TrafficSplitter 执行分发、ResourceGovernor 空闲门控；`PromptVersionStore.OnActivate` 回调热更新 `ImmutableCore`（`task_type='general'`） | M13 §2.5 + M5 §11.0 |
| 全局字典 | HE-Rule-4 数据驱动迭代、MEMF/HeuristicsMemory/GEPA 定义 | 00-Global-Dictionary §2, §9-bis |
| 事件总线协议 | EventHeuristicGenerated（内环规避规则注入）、EventEvalCompleted（外环评测结果驱动 Activate） | internal/protocol/types.go |
| DDL | sys_prompt_versions（Prompt 版本化）、skill_variant_pool（工具描述符变体池）| internal/protocol/schema/010_self_improve.sql |
| 时序图 | KillSwitch 触发链（自进化在 KillSwitch 各阶段的响应）| DIAGRAMS.md#killswitch |

---

## 8. 实现状态与 2026 研究对照

三环架构（`Engine.Run`）、PromptOptimizer、MEMF + HeuristicsMemory（SQLite 降级版）、SurpriseIndex 三组件、L2/L3/L4 触发路径均已实现（`internal/learning/`）。接线要点：Gate 2 `ShadowExecutor` 由 `boot_agent.go` 5 分钟周期触发器驱动（曾因缺顶层触发器被误判死代码删除）；`m9Engine` 与 `LogicCollapseMonitor` 共用同一 `SQLiteRolloutStore`；FastPath 合成感知后异步 `RecordIntentVector`（source="fastpath_synthetic"）。

### 引入计划

| 研究 | 来源 | 核心机制 | 引入点 | 优先级 |
|------|------|---------|-------|-------|
| **D-MEM 多巴胺门控** | arXiv:2603.14597, 2026 | SurpriseIndex 输出接入 M5 Consolidation 情节→语义晋升决策，仅高 surprise 事件触发巩固写入 | `internal/learning/` → `internal/memory/` §9 | P1 |
