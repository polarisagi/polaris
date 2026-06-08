# 模块 12: Eval Harness

> Go | L3 治理层 | [Code-Package-Mapping] → pkg/governance/
> [HE-Rule-4]: Eval 第 0 行存在，失败 = PR 不能合并
> 黄金测试集 + 轨迹回放 + 影子执行 + 回归基线 + 自动熔断
> **§跳读**: 0-bis:7 职责 / 0-ter:19 不变量速查 / 1:32 EvalCase / 2:58 Evaluator5层 / 3:75 轨迹录制 / 4:87 Runner / 5:93 Suite分区 / 6:129 IncidentToEval / 7:138 AutoBootstrap / 8:148 影子执行 / 9:154 连续采样 / 10:168 增量快照 / 11:180 回归检测 / 12:194 集成回放 / 13:210 InvariantTestSuite / 14:237 EvalStore / 15:246 闭环 / 17:252 279(SOFT)降级 / 18:271 依赖
## 0-bis. 职责边界

| M12 **是** | M12 **不是** |
|-----------|-------------|
| EvalCase 管理与执行（L1-L5 五层评测） | 代码的正确性验证（那是 Go test） |
| 轨迹录制（TrajectoryRecorder）与回放（TrajectoryReplayer） | LLM 推理调用（回放时不调 LLM，用录制值） |
| 影子执行对比（baseline vs candidate） | 流量分发（那是 M13 TrafficSplitter） |
| 回归检测（RollingBaseline + RegressionDetector） | 熔断执行（M11 KillSwitch 基于 M12 信号触发） |
| Staging Stage 3-5 评测门控 | Staging Stage 6-7（那是 M11 canary_rollout + full_promotion） |
| DataSplitter 三层分区隔离 | 分区访问权限执行（Holdout Set 由 M11 强制执行） |
| 连续采样监控（1% 流量 + 滑动窗口退化检测） | 自进化候选产出（那是 M9） |

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M12_01 | Eval 失败 = PR 阻塞——Day 1 起 CI 强制执行 | CI `eval_gate` pipeline |
| inv_M12_02 | Holdout Set 与 Training Set Ed25519 签名隔离 + 进程边界强制——M9 不可访问 | M12 §5 双层防护 |
| inv_M12_03 | 重放时不重新调 LLM——TrajectoryReplayer 返回录制值（零 token 消费） | M12 §3 InterceptLLM |
| inv_M12_04 | 评估原语优先级：Assertion > Embedding > LLM-Judge（确定性优先） | M12 §2 五层 Evaluator |
| inv_M12_05 | L4 代码修改须经 process-external CI pipeline——不得在运行进程内评估 Holdout Set | M12 §5 L2 进程边界 |
| inv_M12_06 | 影子执行 candidate 禁止 write_network + privileged——仅 read_only + write_local（隔离影子 workspace） | M9 §2.3 Gate 2 安全护栏 |

---

## 1. EvalCase 结构体

```
EvalCase:
  ID / Name / Description string
  Severity: P0(阻塞) | P1(警告) | P2(记录)
  Source: manual | synthetic | shadow | incident
  Task *Task
  ExpectedOutput string, optional
  ExpectedToolCalls []ExpectedToolCall, optional
  ExpectedState json.RawMessage, optional
  Evaluators []EvaluatorSpec
  Tags []string / CreatedAt int64 / CreatedFrom string(事故ID), optional

ExpectedToolCall:
  ToolName string / Args map[string]interface{} / Mode: exact|subset|contains
```

四种来源:
1. **手工黄金集 (SourceManual)**: 开发者或专家手动编写的基准测试。
2. **合成用例 (SourceSynthetic)**: 由 M9 `EvalGenerator` (基于 RAGAS 进化管线) 离线生成 `SyntheticCase`，由适配器转换为 EvalCase 并仅注入 Training/Validation Set。禁止自动升级为 P0/P1。
3. **影子执行对比 (SourceShadow)**: 通过 M13 影子流量在生产环境捕获的基线对比。
4. **生产事故转换 (SourceIncident)**: 线上 Failure 转换为回归用例。

HITL 门控: Incident-to-Eval 须经 [PIIGuard] 脱敏 + 人工审批方可进入 Holdout Set。

## 2. Evaluator 接口 (5 层)

```
Evaluator: Evaluate(ctx, trajectory, expected) → (EvalResult, error) / Type() → EvaluatorType
EvalResult: Passed bool / Scores map[string]float64 / Details string / EvaluatorType
```

**L1 AssertionEvaluator** — 零 LLM。contains | not_contains | regex | length_under（Unicode 字符数，非字节数）| tool_called | no_tool_called(越界) | cost_under | steps_under。断言失败 → fail(名称+期望值)。

**L2 SchemaEvaluator** — 1. outputSchema → 验证输出 JSON 2. 遍历工具调用 → 验证 Args JSON schema。失败 → schema_violation/tool_args_schema。

**L3 TrajectoryEvaluator** — exact(按序) | subset(Agent⊆参考) | contains(Agent⊇参考)。失败 → mismatched_step/unexpected_tool/missing_tool。

**L4 LLMJudgeEvaluator** — 1. rubric: Task Completion/Tool Correctness/Efficiency/Safety/Communication 各 1-5 分 2. Judge LLM != Agent 模型 3. 解析 JSON 评分 4. 与 PassThreshold 比较。双 Judge 交叉验证，不一致第三 Judge 打破僵局。定期人工校准: Cohen's kappa <0.6 触发，连续 2 周期 <0.4 → 降 L4 权重。

**L5 HumanEvaluator** — 仅校准不门控。每两周抽样 10-20 条(P0/P1/P2 各 1/3)。计算 kappa → 调 rubric → 写 eval_calibration。

## 3. 轨迹录制与回放

**实现**: `pkg/governance/eval/eval.go` （TrajectoryRecorderImpl / TrajectoryReplayerImpl）

`TrajectoryTrace` 包含 LLMCalls / ToolCalls / StateTrans 三类记录。

**TrajectoryRecorderImpl** 从事件日志扫描 `events:session:{id}:` 前缀，按 Type 分流构建 TrajectoryTrace 快照；`RunnerImpl.RunReplay` 直接扫描 Store 验证 offset 连续性和零新 LLM 调用。

**TrajectoryReplayerImpl** 验证规则：状态转移链不断裂（StateTrans[i].From == StateTrans[i-1].To），断裂时返回含 step 位置的 Fail 结果；重放路径不产生新 LLM 调用（zero-token 保证，`new_llm=0`）。

wazero 确定性适配（`clock_time_get`/`random_get` 录制/回放）详见 [Wasm-Sandbox]；M7 按 ctx `eval_mode` 动态注入。CI 回放（毫秒/零费用/确定性）+ Nightly 重执行（真实模型）。

## 4. Eval Runner

EvalRunner/EvalRunConfig/TrajectoryRecorder/TrajectoryReplayer 实现见 `pkg/governance/eval_runner.go`。

CI: PR 变更 `prompts/** skills/** config/** go.mod` → replay P0+P1, 5min 超时。P0 失败阻塞，P1 单 Judge 置信度阈值见 `spec/state.yaml §m12_eval.judge_single_confidence`，低于阈值告警。

## 5. Eval Suite 分区 (防 M9 过拟合)

```
DataSplitter: SourceIncident→Holdout | SourceSynthetic→Training | SourceManual→Holdout(+--allow-training)
三层分区:
  Training Set — M9 (agent_role=m9_optimizer) 可访问，Ed25519 签名隔离
  Validation Set — M9 可访问（受 Ed25519 签名隔离），日常进化在 Validation Set 上做泛化能力评估
  Holdout Set — 仅 CI/Canary (agent_role=ci_gate) 可访问，终态门控
M9 §1.1 PromptOptimizer 早停依据: Training Set 充分性 + Validation Set 泛化性双指标。M9 §3 L0/L1/L2+ 各自的"日常反馈数据源"显式列出。
约束: Training Set ≥200 用例方可 M9 优化
物理存储路径与版本控制（强制约束）:
  Training Set / Validation Set: `~/.polarisagi/polaris/eval/training/` 和 `~/.polarisagi/polaris/eval/validation/`
  Holdout Set: `~/.polarisagi/polaris/eval/holdout/`（主进程副本，仅 CI/Canary 在独立进程内访问）
  禁止将任何 Eval 数据集放入任何 `~/.polarisagi/polaris/workspace/` 子目录。上述 eval 目录必须作为独立的 Git 仓库进行版本控制，与主代码工作区完全隔离。所有的数据集拉取与提交操作统一通过 `polaris eval sync` 专用命令完成。DataSplitter 写入路径由 M11 Immutable Kernel 在启动期 SHA-256 校验，路径错误 → fail-closed。

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

## 6. IncidentToEval (4 阶段)

1. 提取失败模式: 识别出错工具 + 触发输入
2. 定义预期行为: 人工标注正确工具序列和输出
3. 创建 EvalCase: Name="incident-{id}", Source=SourceIncident, L1+L3, Tags=["incident",分类]
4. 写 EvalStore

类型定义见 `pkg/governance/eval/incident.go`（IncidentStore 接口：GetUnresolvedIncidents/GetIncident/MarkConverted；Incident 字段：ID/Title/TriggerInput/FailedToolCall/SeverityLevel/ExpertAnnotation/Status）。

## 7. Auto-Eval-Bootstrapping

触发: 技能黄金用例=0 + System 2 成功 ≥50
1. EpisodicStore 最近 50 次成功 → embedding 余弦最大分散选 5 条
2. LLM-as-Judge 审查: Tier 1+ → Tier 3 强力模型; Tier 0 → Self-Consistency(3 轮多数投票 + 双角色 Safety Auditor/Correctness Verifier) + L1/L2 硬拦截(write_network/file_delete/exec_command + ≥[Taint-Medium] → needs_review) + 禁权(RiskLevel=low, MaxCalls=3)
3. 5 条全过 → EvalCase(SourceSynthetic, auto_bootstrap, zero_day, Severity=P2, needs_human_audit=true)；否则 needs_review
4. 技能 Eval 执行 ≥10 次后 deprecated=true。RiskLevel≥high → HITL

**Severity 约束**: `SourceSynthetic` 自动生成用例 Severity 硬上限为 P2（观察指标），禁止自动设为 P0（阻塞 PR）或 P1（门控警告）。`needs_human_audit` 标志默认 true——需经人工审核确认用例安全性后，手动升级 Severity 并清除标志方可参与 CI 门控。此约束防止系统将含漏洞的成功轨迹（如绕过沙箱的注入技巧）自动固化为黄金标准——若安全模块修复该漏洞，M12 Eval Harness 反而会因"偏离黄金用例"报错阻塞，形成安全反转。编译前安全闸门（M6 §2.2）的静态分析 + L1/L2 硬拦截独立于 Eval 门控，确保安全检查不受 Severity 影响

## 8. 影子执行

ShadowExecutor/ShadowVersion/ComparisonResult/ContinuousSamplingMonitor/RegressionDetector 实现见 `pkg/governance/shadow_executor.go`。

Eval Harness 仅提供对比原语。流量分发由 M9 ProgressiveRollout + M13 TrafficSplitter 管理。

## 9. 连续采样监控

每 10min 计算 100 条采样窗口均值（samplingRate=0.01，degradationThreshold=0.9），avgScore < baselineScore×0.9 触发 SilentDegradationAlert 并执行**归因分析**（Causal Attribution）：

取 7 天前 pre-change baseline 快照，对比当前得分：
- 当前显著低于 baseline 且 baseline 未退化 → **内部回归** → 自动回滚 7 天 L1-L3 产物 → 全量 Eval replay；
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
| L2 新技能 | SQLite WAL + wasm 文件 | MB-数十MB |
| L3 策略/LoRA | 文件拷贝 | MB |
| L4 源码 | 全量 Backup | GB |

引擎: SQLite `sqlite3_backup_init`（在线热备份，WAL 模式）/ 文件 mtime 过滤

## 11. 回归检测

**实现**: `pkg/governance/eval/eval.go`（RegressionDetector.Check）

`RegressionDetector.Check(baseline, current *RunMetrics) *RegressionAlert` 对三个指标执行相对变化率检测：

| 指标 | 触发条件 | 语义 |
|------|---------|------|
| TaskSuccessRate | 下降 > 5%（相对值） | 安全一票否决，无例外 |
| AvgLatencyMs | 上升 > 20%（相对值） | P95 延迟回归 |
| TokenBurnRate | 上升 > 30%（相对值） | 成本软约束 |

任一触发返回 `RegressionAlert{Metric, Baseline, Current, Threshold}`，nil 表示无回归。调用方（M9 外环 Engine.handleEvalCompleted）基于 Alert 决定是否触发 autoRollback。

## 12. 集成轨迹回放

```
FullTrace: TraceID / Task / InterModuleCalls []InterModuleCall / Result / Error
InterModuleCall: Seq/Caller/Callee/Method/Input/Output/Duration/LamportClock/CausalID

IntegrationReplayer.Replay:
  1. 构建偏序依赖图(LamportClock + CausalID)
  2. 接口存在性 — 全序遍历
  3. 输入 Schema — 全序匹配
  4. 偏序因果等价 — CausalID 分组，组内 LamportClock happened-before；不同组交织不比对
  5. 集合最终一致性 — Output 集合一一对应，不要求顺序
```

CI: `go run ./cmd/eval integration-replay --traces testdata/integration/`

## 13. Harness Invariant Test Suite

`pkg/governance/invariant_test.go` 已实现，PR 阻塞级别与 P0 EvalCase 同级，套件受 M11 Immutable Kernel 保护（`ci/safety/`）。

```
TestInvariant1_ObservabilityFirst [HE-Rule-1]:
  完整任务 → 每步 LLM/tool/memory 均有 OTel span + metric 递增

TestInvariant2_VerifiableExecution [HE-Rule-2]:
  3 种 schema 违规 DAGNode → L1 拒绝; 2 种合法 → 放行; 回放 50 条历史轨迹 → 验证一致

TestInvariant5_SeparationOfConcerns [HE-Rule-5]:
  M4↔M1 仅 InferRequest/InferResponse; M11↔M5 仅 SafeString/TaintedString

TestInvariant6_StateMachineControlFlow [HE-Rule-5]:
  LLM 非 JSON → FSM 不 crash, S_REPLAN; LLM 额外 tool_call → PolicyGate 拒绝

TestFullSafetyChain:
  prompt injection → M11 [Taint-High] → M4 SchemaValidator → M11 [Cedar-Gate] 拒绝
  → M7 Capability 委托链拒绝 → [EventLog] 完整拒绝链路

TestPermissionModes:
  验证 `default` / `auto_review` / `full_access` 三种安全模式下，对不同信任等级扩展的安装拦截与运行时危险操作（如 write_network）的 HITL 审批/自动放行逻辑符合 Cedar 策略预期。
```

CI: PR 自动执行，失败 = PR reject(P0 同级)。套件受 M9 Immutable Kernel 保护(`ci/safety/`)。

## 14. EvalStore

```
SaveCase / GetActiveCases / GetCaseByID / MarkDeprecated / Snapshot(version)
后端: [Storage-SQLite]
废弃: 默认标记 deprecated。PII+GDPR Art.17 → HardDeleteForPrivacy 物理擦除(轨迹+chunk+LLM 缓存)，保留匿名 case_id + 删除时间戳审计
CoverageGaps: 输入工具注册表 → 返回未覆盖工具名
```

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

## 18. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M2 Storage | EvalStore 存储（[Storage-SQLite] 后端）、TrajectoryRecorder/Replayer 持久化 | M2 §1.3 |
| M4 Agent Kernel | FSM 轨迹录制（全部状态转移 + LLM 调用 + 工具调用）| M4 §1 |
| M6 Skill Library | Auto-Eval-Bootstrapping（技能黄金用例自动生成）| M6 §2.2 |
| M9 Self-Improve | PromptOptimizer 早停依据（Training Set + Validation Set）、ProgressiveRollout 对比评估 | M9 §1.1, §2.3 |
| M11 Policy Safety | Eval 执行中禁止 M9 访问 Holdout Set（Ed25519 签名隔离 + 进程边界）| M11 §8, M12 §5 |
| 接口定义 | Evaluator/EvalResult/EvalCase/TrajectoryEvent | pkg/governance/evaluator_types.go |
| 全局字典 | HE-Rule-4 数据驱动迭代 | 00-Global-Dictionary §2 |
