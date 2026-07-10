# 模块 12: Eval Harness

> Go | L3 治理层 | [Code-Package-Mapping] → internal/eval/> [HE-Rule-4]: Eval 第 0 行存在，失败 = PR 不能合并
> 黄金测试集 + 轨迹回放 + 影子执行 + 回归基线 + 自动熔断
<!-- §跳读: 0-bis:6 职责 / 0-ter:18 不变量速查 / 1:31 EvalCase / 2:53 Evaluator5层 / 3:70 轨迹录制 / 4:82 Runner / 5:90 Suite分区 / 6:123 IncidentToEval / 7:129 AutoBootstrap / 8:139 影子执行 / 9:145 连续采样 / 10:159 增量快照 / 11:171 回归检测 / 12:179 集成回放 / 13:183 InvariantTestSuite / 14:210 EvalStore / 15:214 闭环 / 17:220 279(SOFT)降级 / 18:245 依赖 -->
## 0-bis. 职责边界

| M12 **是** | M12 **不是** |
|-----------|-------------|
| EvalCase 管理与执行（L1-L5 五层评测） | 代码的正确性验证（那是 Go test） |
| 轨迹录制（TrajectoryRecorder）与回放（TrajectoryReplayer） | LLM（Large Language Model，大语言模型） 推理调用（回放时不调 LLM，用录制值） |
| 影子执行对比（baseline vs candidate） | 流量分发（那是 M13 TrafficSplitter） |
| 回归检测（RollingBaseline + RegressionDetector） | 熔断执行（M11 KillSwitch 基于 M12 信号触发） |
| Staging Stage 3-5 评测门控 | Staging Stage 6-7（那是 M11 canary_rollout + full_promotion） |
| DataSplitter 三层分区隔离 | 分区访问权限执行（Holdout Set 由 M11 强制执行） |
| 连续采样监控（1% 流量 + 滑动窗口退化检测） | 自进化候选产出（那是 M9） |

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M12_01 | Eval 失败 = PR 阻塞——Day 1 起 CI（Continuous Integration，持续集成） 强制执行 | CI `eval_gate` pipeline |
| inv_M12_02 | Holdout Set 与 Training Set Ed25519 签名隔离 + 进程边界强制——M9 不可访问 | M12 §5 双层防护 |
| inv_M12_03 | 重放时不重新调 LLM——TrajectoryReplayer 返回录制值（零 token 消费） | M12 §3 InterceptLLM |
| inv_M12_04 | 评估原语优先级：Assertion > Embedding > LLM-Judge（确定性优先） | M12 §2 五层 Evaluator |
| inv_M12_05 | L4 代码修改须经 process-external CI pipeline——不得在运行进程内评估 Holdout Set | M12 §5 L2 进程边界 |
| inv_M12_06 | 影子执行 candidate 禁止 write_network + privileged——仅 read_only + write_local（隔离影子 workspace） | M9 §2.3 Gate 2 安全护栏 |

---

## 1. EvalCase 结构体

```yaml
EvalCase:
  ID: string
  Name: string
  Description: string
  Input: map[string]any
  Expected: map[string]any
  Level: EvaluatorLevel # (Level1Assert…Level5Human)
  Severity: Severity # P0(阻塞) | P1(警告) | P2(记录)
  Source: string
  Tags: list[string]
  Config: map[string]any
  BehaviorType: BehaviorType # tool_call_sequence | semantic_quality | format_compliance | safety_boundary
  FalsifiabilityScore: float64 # (< 0.5 跳过 L4 LLM Judge)
  NeedsHumanAudit: bool
```

四种来源:
1. **手工黄金集 (SourceManual)**: 开发者或专家手动编写的基准测试。
2. **合成用例 (SourceSynthetic)**: 由 M9 `EvalGenerator` (基于 RAGAS 进化管线) 离线生成 `SyntheticCase`，由适配器转换为 EvalCase 并仅注入 Training/Validation Set。禁止自动升级为 P0/P1。
3. **影子执行对比 (SourceShadow)**: 通过 M13 影子流量在生产环境捕获的基线对比。
4. **生产事故转换 (SourceIncident)**: 线上 Failure 转换为回归用例。

HITL（Human-in-the-loop，人机协同） 门控: Incident-to-Eval 须经 [PIIGuard] 脱敏 + 人工审批方可进入 Holdout Set。

## 2. Evaluator 接口 (5 层)

```yaml
Evaluator:
  Evaluate: (ctx, trajectory, expected) -> (EvalResult, error)
  Type: () -> EvaluatorType

EvalResult:
  Passed: bool
  Scores: map[string]float64
  Details: string
  Type: EvaluatorType
```

- **L1 AssertionEvaluator** — 零 LLM。`contains` | `not_contains` | `regex` | `length_under`（Unicode 字符数，非字节数）| `tool_called` | `no_tool_called`(越界) | `cost_under` | `steps_under`。断言失败 → `fail(名称+期望值)`。
- **L2 SchemaEvaluator** — 1. `outputSchema` → 验证输出 JSON 2. 遍历工具调用 → 验证 Args JSON schema。失败 → `schema_violation/tool_args_schema`。
- **L3 TrajectoryEvaluator** — `exact`(按序) | `subset`(Agent⊆参考) | `contains`(Agent⊇参考)。失败 → `mismatched_step/unexpected_tool/missing_tool`。
- **L4 LLMJudgeEvaluator** — 1. rubric: Task Completion/Tool Correctness/Efficiency/Safety/Communication 各 1-5 分 2. 默认采用 DeepSeek V4 担任 Judge LLM（其极低 API 成本使得全量双 Judge 交叉验证、三 Judge 打破僵局成为经济且默认的配置，无需再因成本顾虑而降级为单 Judge 抽样） 3. 解析 JSON 评分 4. 与 PassThreshold 比较。双 Judge 交叉验证，不一致第三 Judge 打破僵局。定期人工校准: Cohen's kappa <0.6 触发，连续 2 周期 <0.4 → 降 L4 权重。
- **L5 HumanEvaluator** — 仅校准不门控。每两周抽样 10-20 条(P0/P1/P2 各 1/3)。计算 kappa → 调 rubric → 写 `eval_calibration`。

## 3. 轨迹录制与回放

**实现**: `internal/eval/harness/`（TrajectoryRecorderImpl / TrajectoryReplayerImpl）

`TrajectoryTrace` 包含 LLMCalls / ToolCalls / StateTrans 三类记录。

**TrajectoryRecorderImpl** 构造时需注入 `protocol.Store`（`NewTrajectoryRecorder(store)`）；`Record()` 扫描 `events:session:{id}:` 前缀，按 Type 分流：
- `llm_call/inference_request` → LLMCalls
- `action_pending/action_done` → ToolCalls
- 其余 → StateTrans

**TrajectoryReplayerImpl** 验证规则：状态转移链不断裂（StateTrans[i].From == StateTrans[i-1].To），断裂时返回含 step 位置的 Fail 结果；重放路径不产生新 LLM 调用（zero-token 保证，`new_llm=0`）。

## 4. Eval Runner

`RunnerImpl` 及评测器接口定义分布于 `internal/eval/` 根目录与 `internal/eval/harness/`。

**`internal/eval/harness/` 核心文件**：
- `runner.go`: L1-L5 Evaluator + RunnerImpl
- `sampling_monitor.go`: 连续采样监控（1% 流量滑动窗口退化检测）
- `founding_anchor.go`: FoundingAnchor 演进保护
- `synthetic_adapter.go`: L3→L2 转换（SyntheticCase→EvalCase）
- `store.go`: SQLiteEvalStore

**✅ 组件状态更新（2026-07-10）**：`ShadowExecutor`（`internal/eval/analysis/shadow_executor.go`）已恢复实现并完成生产接入——此前经历"实现→误删→重新实现→再次误删为死代码"的反复（详见 ADR-0029 §K 修订记录），根因是仅有实现、缺少顶层周期触发器与 `StagingPipeline` 未接入 M9Engine 两处集成缺口，而非设计本身有问题。现由 `cmd/polaris/boot_agent.go` 启动 5 分钟周期 goroutine，发现 `rollout_states` 中停留在 Gate 2(Shadow) 的候选并调用 `RunReplayBatch`。

漂移分数监控已实现为 `internal/eval/founding_anchor.go` 的 `DriftMonitor` 结构体（`atomic.Value` 封装，非包级全局变量），符合 ADR-0001（Architecture Decision Record，架构决策记录） 规定。

**CI 门控**:
- PR 变更 `prompts/** skills/** config/** go.mod` → replay P0+P1，5min 超时。
- P0 失败阻塞，P1 单 Judge 置信度阈值见 `spec/state.yaml §m12_eval.judge_single_confidence`，低于阈值告警。
- `BlockDeploy = P0PassRate < 1.0`
- `WarnDeploy = P1PassRate < 0.8`

## 5. Eval Suite 分区 (防 M9 过拟合)

```
DataSplitter: SourceIncident→Holdout | SourceSynthetic→Training | SourceManual→Holdout(+--allow-training)
三层分区:
  Training Set — M9 (agent_role=m9_optimizer) 可访问，Ed25519 签名隔离
  Validation Set — M9 可访问（受 Ed25519 签名隔离），日常进化在 Validation Set 上做泛化能力评估
  Holdout Set — 仅 CI/Canary (agent_role=ci_gate) 可访问，终态门控
M9 §1.1 PromptOptimizer 早停依据: Training Set 充分性 + Validation Set 泛化性双指标。M9 §3 L0/L1/L2+ 各自的"日常反馈数据源"显式列出。
约束: Training Set ≥200 用例方可 M9 优化
存储实现：SQLiteEvalStore（`internal/eval/harness/`），基于 `protocol.Store` KV 后端。Key 格式：`eval:case:{partition}:{agentRole}:{caseID}`。分区隔离通过 Ed25519 签名 + policy.CheckAccess 双重执行，无独立 Git 仓库。

隔离: 三层防护
  L1 (API 层): Ed25519 签名 X-Eval-Source header
    M9(agent_role=m9_optimizer) → Training Set + Validation Set
    CI/Canary(agent_role=ci_gate) → Holdout Set
  L2 (进程边界, 强制执行): L4 代码修改生成的 PR 必须通过 process-external CI pipeline 验证——不得在运行进程内执行 Holdout Set 评估。CI runner 为独立进程，使用独立密钥签名的 Holdout Set 副本，M9 不可访问 CI runner 的文件系统。
  L3 (文件系统层): M7 workspace_read 显式拒绝 `~/.polarisagi/polaris/eval/holdout/`（与 `~/.polarisagi/polaris/config/` 等并列，见 M7 §4.5）；M11 Cedar Layer 2 forbid 规则阻止 Agent role 读取 `context.polarisagi/polaris_eval_holdout_path`（见 M11 §3）；`bash_restricted` 硬编码禁止 `~/.polarisagi/polaris` 全路径（已有，兜底）。
  Holdout Set 完整性和 CI 配置受 M11 Immutable Kernel (`ci/safety/`) 保护，L4 白名单不含此路径。
```

### 5.1 Eval API 接口定义

为支持 M9 (PromptOptimizer 等) 合法访问 Training Set 和 Validation Set，M12 暴露只读的 Eval API。该 API 绕过 M7 Workspace Bridge，作为内部 L1 隔离通道。

- **接口声明**: `internal/protocol/interfaces.go` 中的 `EvalAPI` 接口。
- **签名验证**: 调用方需提供基于自身 role 的 Ed25519 签名。M9 需使用 `agent_role=m9_optimizer` 对应的私钥进行签名，M12 验证签名后放行对 Training/Validation 分区的读取，拦截对 Holdout 分区的读取。

**EvalAPI 方法清单**（完整签名见 `internal/protocol/interfaces.go:EvalAPI`）:
- `GetTrainingCases(role, sig) → []EvalCase` — 训练用例，签名验通过后放行 Training 分区
- `GetValidationCases(role, sig) → []EvalCase` — 验证用例，放行 Validation 分区
- 两者均拒绝 Holdout 分区访问；signature = Ed25519 over (params + timestamp)

## 6. IncidentToEval

实现：`internal/eval/harness/`（`IncidentToEvalConverter`）。

流程：IncidentPayload（含 Input/Expected/taint_level）→ PII（Personally Identifiable Information，个人敏感信息） 脱敏（PIIDetector 接口）→ 写 `pending_review` 分区（Level4LLMJudge, SeverityP0）。高污点（taint≥3）或 needs_human_audit=true 的事件拒绝自动转换，要求人工审核。`ReviewAndPromote()` 提供 HITL 审批入口，确认后迁移至 validation 分区。

## 7. Auto-Eval-Bootstrapping

触发: 技能黄金用例=0 + System 2 成功 ≥50
1. EpisodicStore 最近 50 次成功 → embedding 余弦最大分散选 5 条
2. LLM-as-Judge 审查: 得益于 DeepSeek API 的极高性价比，全 Tier 默认启用高度冗余的强校验 Self-Consistency (5 轮多数投票 + 双角色 Safety Auditor/Correctness Verifier) 以确保合成用例质量。附带 L1/L2 硬拦截(write_network/file_delete/exec_command + ≥[Taint-Medium] → needs_review) + 禁权(RiskLevel=low, MaxCalls=3)
3. 5 条全过 → EvalCase(SourceSynthetic, auto_bootstrap, zero_day, Severity=P2, needs_human_audit=true)；否则 needs_review
4. 技能 Eval 执行 ≥10 次后 deprecated=true。RiskLevel≥high → HITL

**Severity 约束**: `SourceSynthetic` 自动生成用例 Severity 硬上限为 P2（观察指标），禁止自动设为 P0（阻塞 PR）或 P1（门控警告）。`needs_human_audit` 标志默认 true——需经人工审核确认用例安全性后，手动升级 Severity 并清除标志方可参与 CI 门控。此约束防止系统将含漏洞的成功轨迹（如绕过沙箱的注入技巧）自动固化为黄金标准——若安全模块修复该漏洞，M12 Eval Harness 反而会因"偏离黄金用例"报错阻塞，形成安全反转。编译前安全闸门（M6 §2.2）的静态分析 + L1/L2 硬拦截独立于 Eval 门控，确保安全检查不受 Severity 影响

## 8. 影子执行

**✅ ShadowExecutor 已实现（2026-07-10 恢复接入）**。`ShadowExecutor.RunReplayBatch` 从 `events` 表按游标增量抓取 `llm_call` 记录，对候选配置回放打分（异步 EventLog 回放，ADR-0029 §K 决策），通过率 ≥ `M12Eval.ShadowPassRateThreshold` 调用 `StagingPipeline.ConfirmShadow` 推进 Gate 2→3，否则调用 `Rollback`。`ConfirmShadow` 内部进一步通过 `promptActivator` 回调 `PromptVersionStore.Activate`，这是 M9 GEPA 候选 Prompt 真正生效的唯一入口——此前 `handleEvalCompleted` 在 Eval(Gate 1) 一通过就同步调用 `Activate`，Gate 2/3 从未真正拦截过任何候选，已随本次修复一并纠正。

ContinuousSamplingMonitor ✅ 已实现于 `internal/eval/analysis/sampling_monitor.go`，滑动窗口 100 条 + 10min 定时检测 + 退化归因（Internal/External/Mixed），`onDegradation` 回调由 M9 注入——该项与 ShadowExecutor 相互独立。

Eval Harness 仅提供对比原语。流量分发由 M9 ProgressiveRollout + M13 TrafficSplitter 管理。

## 9. 连续采样监控

每 10min 计算 100 条采样窗口均值（samplingRate=0.01，degradationThreshold=0.9），avgScore < baselineScore×0.9 触发 SilentDegradationAlert 并执行**归因分析**（Causal Attribution）：

取 7 天前 pre-change baseline 快照，对比当前得分：
- 当前显著低于 baseline 且 baseline 未退化 → **内部回归** → 触发 M9 autoRollback：L0/L1 候选直接切回旧 Baseline 版本；L2+ 候选重新入 Staging 流水线 Stage 1（不允许绕过 Staging 流程直接降级），同时触发全量 Eval replay；
- 两者同比例退化（差分 < 5%）→ **外部因素**（Provider 降级/网络抖动）→ 仅 Alert，抑制自动回滚；
- 两者退化比例不一致（差分 ≥ 5%）→ 混合因素 → 保守抑制回滚 + HITL；
- 归因超时（> 60s）→ 同上保守策略。

此机制防止 Provider 降级引发级联回滚风暴。归因期间冻结 M9 Auto-Curriculum。

影子=部署前版本对比(version change)；连续采样=部署后趋势监控(score degradation)。共享 LLM Judge 引擎。

## 10. 增量快照

| 候选 | 快照内容 | 大小 |
|-----|---------|------|
| L0 配置 | SQLite 单表导出 | KB |
| L1 Prompt | 文件+表导出 | KB-MB |
| L2 新技能 | SQLite WAL + 脚本文件 | MB-数十MB |
| L3 策略/LoRA | 文件拷贝 | MB |
| L4 源码 | 全量 Backup | GB |

引擎: SQLite `sqlite3_backup_init`（在线热备份，WAL 模式）/ 文件 mtime 过滤

## 11. 回归检测

**实现**: `internal/eval/harness/`（RegressionDetector.Check）

`RegressionDetector.Check(baseline, current *RunMetrics) *RegressionAlert` 对三项指标执行 **30 天滚动窗口相对百分比**检测：TaskSuccessRate 下降 >5% 触发，AvgLatencyMs 上升 >20% 触发，TokenBurnRate 上升 >30% 触发（成本软约束）。返回 `RegressionAlert{Metric, Baseline, Current, Threshold}`，nil 表示无回归。**注意：结构体不含 Level / Action 字段**，调用方（M9 外环）自行决策后续动作，`RegressionDetector` 本身无侧效应。

**连续采样退化告警**：`ContinuousSamplingMonitor.CheckDegradation()` 返回 `(degraded bool, alert *DegradationAlert)`；degraded=true 时触发 `slog.Warn("SilentDegradationAlert")` 并调用注入的 `onDegradation` 回调（由 M9 注入冻结 Auto-Curriculum + 回滚链逻辑）。

## 12. 集成轨迹回放

设计预留，当前未实现（IntegrationReplayer 不存在于代码库）。现有跨模块回放通过 TrajectoryReplayer（§3）在单进程内完成。

## 13. Harness Invariant Test Suite

`internal/eval/` 中的不变量测试套件已实现，PR 阻塞级别与 P0 EvalCase 同级，套件受 M11 Immutable Kernel 保护（`ci/safety/`）。

- **TestInvariant1_ObservabilityFirst** [HE-Rule-1]: 完整任务 → 每步 LLM/tool/memory 均有 OTel（OpenTelemetry） span + metric 递增
- **TestInvariant2_VerifiableExecution** [HE-Rule-2]: 3 种 schema 违规 DAGNode → L1 拒绝; 2 种合法 → 放行; 回放 50 条历史轨迹 → 验证一致
- **TestInvariant5_SeparationOfConcerns** [HE-Rule-5]: M4↔M1 仅 InferRequest/InferResponse; M11↔M5 仅 SafeString/TaintedString
- **TestInvariant6_StateMachineControlFlow** [HE-Rule-5]: LLM 非 JSON → FSM 不 crash, S_REPLAN; LLM 额外 tool_call → PolicyGate 拒绝
- **TestFullSafetyChain**: prompt injection → M11 [Taint-High] → M4 SchemaValidator → M11 [Cedar-Gate] 拒绝 → M7 Capability 委托链拒绝 → [EventLog] 完整拒绝链路
- **TestPermissionModes**: 验证 `default` / `auto_review` / `full_access` 三种安全模式下，对不同信任等级扩展的安装拦截与运行时危险操作（如 write_network）的 HITL 审批/自动放行逻辑符合 Cedar 策略预期。

CI: PR 自动执行，失败 = PR reject(P0 同级)。套件受 M11 Immutable Kernel 保护(`ci/safety/`)。

## 14. EvalStore

实现：`internal/eval/harness/`（SQLiteEvalStore）。基于 `protocol.Store` KV（Key-Value，键值） 接口（Scan/Get/Put/Delete）。提供 GetTrainingCases / GetValidationCases（Ed25519 签名校验）、PutCase（含分区校验）、PromotePendingCase（pending_review→validation 迁移）、GetPassRateAvgSince（时间窗口通过率统计）。

## 15. 闭环

生产数据 → 失败标注 → Eval 生成 → CI 门控 → 回归检测 → 自动熔断 → 生产数据。

---

## 17. 降级与失败模式

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| 评测器故障 (L1-L5) | blocking staging (fail-closed)，不晋升 candidate | 评测器修复后重新 evaluation |
| 影子执行超时 (>5min) | 标记 Timeout + 跳过该 case | 下次 staging 重新执行 |
| Holdout Set Ed25519 签名校验失败 | 拒绝访问 + CRITICAL 审计 | 密钥管理员重新签名 |
| CI runner 不可达 | 不晋升（进程边界强制隔离） | CI 恢复后重跑 |
| TrajectoryReplayer 回放耗尽 | ErrReplayExhausted → 该 case 跳过 | — |
| RegressionDetector 检测到退化 | autoRollback + AlertCritical | M9 重新生成候选 |
| 连续采样滑动窗口 < 10 samples | 不触发退化检测（统计意义不足） | 积累样本后自动启动 |

与 OSMemoryGuard 协同: L1 预警 → 暂停影子执行 (L2+ 变更) / L2 紧急 → 暂停全部 Eval / L3 临界 → 仅 CI 门控保留。


## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m12_eval`。

## 17-bis. 已知 Bug 修复记录

| 级别 | 文件 | 函数 | 问题描述 | 修复 |
|------|------|------|---------|------|
| P2 | `internal/eval/harness/` | `evaluate` | 第一次 `agent.Run()` 用 `_` 丢弃 `toolNames`；若 `Expected["tools"]` 存在则发起第二次 `agent.Run()`，导致 Agent 被执行两次（重复 LLM 调用、非幂等副作用） | 改为一次调用同时捕获 `output, toolNames, err` |

## 18. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M2 Storage | EvalStore 存储（[Storage-SQLite] 后端）、TrajectoryRecorder/Replayer 持久化 | M2 §1.3 |
| M4 Agent Kernel | FSM（Finite State Machine，有限状态机） 轨迹录制（全部状态转移 + LLM 调用 + 工具调用）| M4 §1 |
| M6 Skill Library | Auto-Eval-Bootstrapping（技能黄金用例自动生成）| M6 §2.2 |
| M9 Self-Improve | PromptOptimizer 早停依据（Training Set + Validation Set）、ProgressiveRollout 对比评估 | M9 §1.1, §2.3 |
| M11 Policy Safety | Eval 执行中禁止 M9 访问 Holdout Set（Ed25519 签名隔离 + 进程边界）| M11 §8, M12 §5 |
| 接口定义 | Evaluator/EvalResult/EvalCase/TrajectoryEvent | internal/eval/harness/（唯一定义处）；internal/eval/ 根目录为门面层（见 §4） |
| 全局字典 | HE-Rule-4 数据驱动迭代 | 00-Global-Dictionary §2 |
