# Polaris — 路线图与工程纪律

> 时间敏感项 / 工程现状 / 未完成研究方向 / 工程纪律 / 拒绝清单。AI 默认不加载;按场景定向读。
> **§跳读**: 1:9 时间敏感 / 2:43 工程现状 / 3:57 未完成研究方向 / 4:83 工程纪律 / 4.3:108 反模式SSoT / 5:151 拒绝清单SSoT
> 近期更新见 git log。

---

## 1. 时间敏感项

| 项 | 触发 | 责任 | 动作 |
|----|-----|------|------|
| 默认 Provider 商业窗口变更 | Provider 发布折扣到期 / 价格调整 / 模型 EOL 公告 | M1 + M12 | 触发标准化路由 benchmark 重评（见 §1.1） |
| 默认 Provider Adapter 模型名升级 | Provider 公告新模型且旧名进入弃用期 | M1 | 短名→新名映射；旧名称 90 天过渡期 fallback；具体映射见 `pkg/substrate/inference/adapter_*.go` |

> **当前默认 Provider**：`configs/defaults.toml` 选 DeepSeek V4 系列。商业窗口与模型名变更跟随上游 Provider 官方公告。

### 1.1 标准化路由 Benchmark

使用开源项目 **promptfoo** (MIT) 作为基准评测框架，结合 M12 Eval Harness 黄金集。

**评测维度**：

| 维度 | 指标 | 阈值 |
|------|------|------|
| 延迟 | P50 / P95 / P99 latency | P95 < baseline × 1.5 |
| 成功率 | HTTP 2xx 占比 | < 95% → 降权 |
| 成本 | USD / 1M tokens (input + output) | 按 provider×model 独立核算 |
| 质量 | M12 黄金集 pass_rate | < baseline × 0.95 → 降权 |

**Benchmark 流程**：

```
1. M12 黄金集选取: ≥30 条覆盖 task_type 分布 (factual_lookup/how_to/temporal_reasoning/complex_reasoning)
2. promptfoo 配置: 每个 provider×model 组合作为独立 eval provider
3. 并行评测: 同一 prompt 发给所有候选 provider，记录 latency + cost + response
4. M12 自动评分: L1 Assertion + L2 Schema + L4 LLM-Judge (Tier 3 模型)
5. 输出路由权重建议: HealthScorer 更新 (可用性×0.4 + 延迟×0.3 + 成本×0.2 + 质量×0.1)
```

**集成**: `promptfoo eval --config testdata/benchmark/routing/providers.yaml` → JSON → M12 EvalStore → RollingBaseline。CI: `make benchmark-routing`（手动触发）。**调度**: 每月 1 日自动；Provider 弃用/升级 / 商业窗口变更时手动触发。

---

## 2. 工程现状

13 模块架构选型与后端代码实施已于 **2026-05-20 全部闭环**（276 个 Go 源文件，202 项自动化测试通过）。`pkg/` 完全对齐 `spec/state.yaml` + DDL + 本架构体系，物理隔离、Taint Tracking、SurpriseIndex、Edge Streaming 等关键能力均已落地。

已落地能力清单（按层）：

- **L0 substrate**: Inference Router + CircuitBreaker + SemanticCache；SQLite + SurrealDB-Core（KV/Vec/Graph/FTS）+ PebbleDB；OTel + TokenBurnRate + SurpriseIndex + AutoConfig + FeatureGate；Cedar Policy + TaintTracking + AuditTrail + PIIDetector（Tier0 正则 + Tier1+ Presidio 门控）
- **L1 cognition/action**: Agent FSM（10 态）+ DAG Executor + ContextAssembler + 四层记忆 + Consolidation；Rust 脚本沙箱 + MCP Client/Manager + CapabilityToken + CodeAct + LAM + Computer Use（Darwin/Linux/Windows）+ Skill Pipeline
- **L2 swarm**: Blackboard（SQLite）+ CAS 协调 + Reaper + Supervisor Tree；SurpriseIndex Calculator + MEMF + GraphRAG（Tier0: Mini-Batch K-Means / Tier1+: DBSCAN）+ Reflexion + Rollout + PromptOptimizer；QLoRA Adapter（FeatureQLoRA 门控）+ PRM + Swarm Topology
- **L3 governance/edge**: EvalCase + 五层 Evaluator + TrajectoryRecorder/Replayer + ShadowExecutor + 合成数据生成；HTTP/SSE + HITL Gateway + TaskQueue + ResourceGovernor + CLI

HT0 基线（`configs/defaults.toml`，tier=0, max_agents=3）已对齐，FeatureGate 自动化覆盖 L3 microVM / QLoRA / LogicCollapse / GraphRAGFull / PresidioPII / WebUI 六项能力的硬件门控（详见 M03 §5）。

---

## 3. 未完成研究方向

> 以下均有明确前置条件，条件未满足前不进实施队列。已实现能力见 §2。

### 3.1 SurpriseIndex Layer B（马尔可夫转移矩阵）

当前 `pkg/swarm/surprise.go` 实现 Layer A/C 组合（embedding cosine + toolSeq + MEMF）。Layer B 为马尔可夫转移矩阵条件概率，定位为全 Provider 可用的控制流主信号。

**前置条件**: Layer A 旁路收集 ≥6 个月数据 → M12 评测增量收益显著 → 升为 stable 替换主信号。详见 `spec/state.yaml §signals.surprise_index`。

### 3.2 形式化验证（Lean4 / TLA+）

覆盖 audit log 不可关、kill switch 不可绕过两条安全不变量。**前置条件**: 系统达到可商用质量。

### 3.3 Late-Interaction 神经重排

M10 HybridRetriever 补充 ColBERT 级别的 Late-Interaction 重排层。**前置条件**: Go ONNX 生态（onnxruntime-go）达到生产可用。

### 3.4 跨节点 Agent

永远不在计划。违反单机硬约束（单进程主体，禁独立进程 DB，禁 sidecar）。

---

## 4. 工程纪律

### 4.1 七筛方法论（适用所有外部材料和技术提案）

1. **剥离比喻保留机制** — 神经科学/昆虫学/热力学比喻不入文档，只取可计算机制
2. **拒绝未验证算法引用** — 必须引用基准结果或可复现实验
3. **拒绝绝对化口号** — "0% CPU"/"零 token"/"零签名"不进设计文档
4. **拒绝魔数进设计文档** — 阈值参数化，由数据和 benchmark 决定
5. **可观测信号必须有可计算定义** — "智能"/"质量"/"异常"必须有具体公式
6. **MVP 保守，研究分支激进** — 好想法未过筛时进研究方向清单，不阻塞 MVP
7. **架构健壮性来自决议层稳定** — 决议层不动，只动参数和默认值

### 4.2 元层判断准则

- **架构原则比技术选择持久** — 决议层半年内不应被新技术冲击；只有参数与默认值会动
- **同一来源反复提议不构成重新引入理由** — 已经七筛筛掉的，后续重提仍拒
- **概念分解 ≠ 代码组织** — 13 模块对应 6 包是有意为之，不要互相替代
- **可计算的小信号 > 不可计算的大概念** — SurpriseIndex 三组件具体，笼统指标不可用
- **MVP 保守，研究激进** — 好想法没有过七筛时进研究方向清单
- **EventLog 是真相源** — 任何派生引擎理论上可重建，这一性质必须在测试中验证
- **横切关切独立成包** — M3/M11/M12 通过接口注入而非重复实现
- **物理隔离 > 提示词加固** — 安全防御只接受可以"物理证伪"的机制（slot/sandbox/capability/audit）

### 4.3 完整反模式速查（跨模块共识，权威源）

控制流与 LLM 边界：
- 让 LLM 直接驱动控制流（违反不变量 6）
- 让 LLM 直接调工具（应由 Kernel 状态机分派）
- 重放时重新调 LLM（应记录值，确定性）
- 用 wall clock 做全序/退避/时间窗（NTP 漂移破坏）
- 自由 NL 多 agent 对话（60-66% 失败率，token 破产）

数据与持久化：
- 不可逆动作自动 rollback（物理上不可逆，只能告警 + 人工）
- 默认 forgetting = 物理删除（应默认归档，GDPR 例外）
- 跨引擎"同步事务"假象（不可实现，用 outbox + 最终一致）
- audit log 允许 DELETE/UPDATE（append-only 硬约束 + DB 触发器）
- 归档不物理删除撑满磁盘（M5 磁盘水位触发 Cold 数据淘汰）
- dead_letter 无具体处理流程（M2 §14.4 处理规范）

安全与边界：
- 抓取内容直接进 system prompt（必须 Taint=High 隔离到 data slot）
- 把 high-taint 直接拼进 instruction slot（物理 slot 分离破坏）
- 污点只升不降无受控降级路径（M11 Taint Graduation）
- builtin tool/系统密钥不走 capability（无后门原则）
- HTTP 默认绑定 0.0.0.0（单机优先，远程显式）
- 不可逆动作仅告警+人工不做预执行验证（M7 DryRunMode）

工程纪律：
- 价格表/魔数写进设计文档（参数化，由数据决定）
- 神经科学/昆虫学比喻泄露到代码层（只取机制，不取命名）
- 把概念模块和代码包混为一谈（13 概念 vs 6 包，服务不同目的）
- SurpriseIndex Day 1 直接入熔断器（必须先有可计算定义，先旁路观测）
- 影子执行全量快照拖慢自学习闭环（M12 增量快照）
- 各模块后台 worker 无全局内存背压协调（M3 OSMemoryGuard 三级水位线统一管辖）

基础设施约束：
- 引入 Redis/Kafka/RabbitMQ（单机优先，违反硬约束）
- 用 cgroups 做单机 App 内存隔离（macOS/Windows 无此机制，进程内 Governor）
- 将图向量存储强制融合为单引擎（2026 无成熟嵌入式 Vector-Graph DBMS）
- 用 DID 替代 OIDC 做 MCP 认证（无成熟生产实现）
- 用"动态功能验证"替代结构化 JSON Schema（违反不变量 6）

---

## 5. 主动决议拒绝清单

> 反复被外部材料提议但本项目明确不采纳。同一来源反复提议不构成重新引入理由（七筛第 7 条）。

| 提议 | 拒绝理由 |
|------|---------|
| 神经科学比喻（LTP/LTD/突触可塑性） | 七筛第 1 条。已采纳机制本身（使用驱动的边权重 + 时间衰减），不引入命名 |
| 昆虫学比喻（Stigmergy/信息素） | 同上。已采纳"共享状态隐式协调"机制 |
| 调质浓度池 / Neuromodulator | 浪漫但不可工程化，Policy 层 stress-mode 切换可替代 |
| 4K Token 工作记忆硬上限 | 七筛第 4 条魔数禁令 |
| 零签名自发现 MCP | 七筛第 3 条，与 Capability 冲突 |
| 0% CPU 空闲占用 | 同上，以"空载 CPU < 1%"可测量目标替代 |
| Inviolable（不可侵犯） | 同上，改为 High-Assurance（高保证），承诺"突破必留痕" |
| 15 模块拆解（独立 Metacognition/WorldModel/Personalization/UI） | 过度分解，功能已分配在 13 模块体系 |
| 5 大挽具层收敛 | 概念分解 ≠ 代码组织，13 概念 + 6 包共存有意为之 |
| DID 替代 OIDC 做 MCP 认证 | 无成熟生产实现 |
| 图向量统一引擎 | 2026 无成熟嵌入式 Vector-Graph DBMS |
| Schema 过时 / 动态功能验证替代 JSON Schema | 违反不变量 6（状态机持有控制流） |
| 引入 Redis/Kafka/RabbitMQ | 违反单机硬约束 |
| cgroups 做单机 App 内存隔离 | macOS/Windows 无此机制，应用进程内 Governor |
| 已集成多模态 / M09 自主补全引擎 | M15 已在底层及前端支持图片、视频分析与 TTS；M09 已实现缺失能力自主探测与合成 |

---

**END OF ROADMAP.md** — 与 [ARCHITECTURE.md](./ARCHITECTURE.md) 配套阅读。
