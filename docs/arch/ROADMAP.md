# Polaris — 路线图与工程纪律

> 时间敏感项 / 工程现状 / 未完成研究方向 / 工程纪律 / 拒绝清单。AI 默认不加载;按场景定向读。当前基准:2026-10。
<!-- §跳读: 1:9 时间敏感 / 2:43 工程现状 / 3:57 未完成研究方向 / 4:83 工程纪律 / 4.3:108 反模式SSoT / 5:151 拒绝清单SSoT -->
> 近期更新见 git log。

---

## 1. 时间敏感项

| 项 | 触发 | 责任 | 动作 |
|----|-----|------|------|
| 默认 Provider 商业窗口变更 | 折扣到期 / 价格调整 / 模型 EOL 公告 | M1 + M12 | 触发标准化路由 benchmark 重评（§1.1） |
| 默认 Provider 模型名升级 | 新模型发布且旧名进入弃用期 | M1 | 短名→新名映射，旧名 90 天过渡 fallback；映射见 `internal/llm/adapter/` |

> 默认 Provider：`configs/defaults.toml` 选 DeepSeek V4 系列（选型与路由语义见 ADR-0020）。

### 1.1 标准化路由 Benchmark

框架：**promptfoo**(MIT) + M12 黄金集（≥30 条，覆盖 `task_type` 分布 `factual_lookup / how_to / temporal_reasoning / complex_reasoning`）。每个 `provider×model` 作为独立 eval provider，同一 prompt 并行发送，M12 自动评分（L1 Assertion + L2 Schema + L4 LLM-Judge）。

| 维度 | 指标 | 阈值 |
|------|------|------|
| 延迟 | P50 / P95 / P99 | P95 < baseline × 1.5 |
| 成功率 | HTTP 2xx 占比 | < 95% → 降权 |
| 成本 | USD / 1M tokens (in+out)，按 provider×model 核算 | — |
| 质量 | M12 黄金集 pass_rate | < baseline × 0.95 → 降权 |

输出路由权重建议：HealthScorer = 可用性×0.4 + 延迟×0.3 + 成本×0.2 + 质量×0.1。入口 `make benchmark-routing`（配置 `testdata/benchmark/routing/providers.yaml`，结果入 M12 EvalStore → RollingBaseline）；每月 1 日 + Provider 弃用/升级/商业窗口变更时手动触发。

---

## 2. 工程现状

13 模块架构与后端实施已于 2026-05-20 闭环,此后为增量演进。`internal/` 对齐 `spec/state.yaml` + DDL + 本架构体系。2026-10 的形态:

- **主形态**:桌面版 Tauri v2 外壳 + Go 守护进程 sidecar,CLI 为同一守护进程的薄客户端(ADR-0096);`internal/cli` 已不存在。项目模型 ADR-0097。
- **Agent**:FSM 14 态(含 S_RESPOND,ADR-0098),DAG 执行引擎独立为 `internal/execute`(ADR-0046)。
- **推理**:云端 Provider 为默认;本地推理默认关闭,embedding 走进程内 ONNX(ADR-0109);语音三档硬件分级(ADR-0110);token 经济 cache-first 前缀 + 账本(ADR-0105)。
- **扩展与互联**:统一信任扩展模型(ADR-0103);跨节点委派 = MCP 门面 + A2A 1.0(ADR-0111)。
- **治理**:KillSwitch / Cedar / Taint 五级 / AuditTrail / 沙箱三档回退均已落地;质量门控体系见 ADR-0091。
- 硬件门控:`configs/defaults.toml` 基线 tier=0, max_agents=3;FeatureGate 覆盖 L3 microVM / QLoRA / LogicCollapse / GraphRAGFull / PresidioPII / WebUI(M03 §5)。

---

## 3. 未完成研究方向

> 以下均有明确前置条件,条件未满足前不进实施队列。已实现能力见 §2。

### 3.1 SurpriseIndex Layer B（马尔可夫转移矩阵）【代码已实现,等待数据积累转 stable】

`MarkovMatrix`(`internal/learning/surprise/surprise_markov.go`)已接入 `SurpriseCalculator`(`internal/learning/surprise/surprise.go`):在线积累转移数据(启动时由 `session_trajectory` 工具序列 warm-start,ADR-0104 决策七);累计转移次数达 `layerBThreshold` 后自动由 Tier-0 启发式切换为马尔可夫条件概率惊异值。阈值 `DefaultLayerBThreshold=1000`、`MinLayerBThreshold=500`(以代码为准;对应 M09 §2.0 的 500+ 成功轨迹)。剩余为运营性前置:生产积累到阈值 + M12 评测验证增量收益后转 stable 主信号。详见 `spec/state.yaml §signals.surprise_index`。

### 3.2 形式化验证（Lean4 / TLA+）

覆盖 audit log 不可关、kill switch 不可绕过两条安全不变量。**前置条件**:系统达到可商用质量。仓库无 `.tla`/`.lean` 文件,未启动。

### 3.3 Late-Interaction 神经重排【近似版已接入 DeepRAG 链路,ONNX 版未启动】

`ApproximateColBERTReranker`(`internal/store/search/reranker.go`,n-gram 窗口 + Embedder 近似 ColBERT MaxSim)经 `cmd/polaris/boot_knowledge.go` 在 FeatureDeepRAG 门控下注入 `internal/knowledge` 的 HybridRetriever;默认 `NilReranker` 透传。真正的 ColBERT-ONNX 版前置条件(Go ONNX 生态生产可用)未满足,届时替换近似版。

### 3.4 跨节点 Agent

「一个 Agent 分布在多台机器上」(共享状态、跨机调度、集群)永远不在计划:违反单机硬约束(单进程主体,禁独立进程 DB,禁 sidecar 式数据面)。

用户多台电脑互相委派任务的新需求由 ADR-0111 解决:**多个各自独立的单进程节点经协议委派**,记忆、黑板、审批互不共享,节点间只交换「任务文本 + 结果」;不跨机同步数据库,不引消息中间件。

### 原生多模态输入与 Computer Use（候选,来源 GD-14-002）

前置条件(缺一不可):
1. Tier-0(2GB)实测单帧图片经 protocol 事件流 → PromptBuilder → Provider 的峰值内存与 token 开销;超限则整体走硬件门控(Tier1+),不得作硬依赖。
2. Agent 侧文件产物管理链路就绪(见 M07 workspace_write 实现状态)。
3. Provider 视觉能力探测:不支持则需按 role 分流到第二 Provider,牵动模型角色配置。

2026-08-12 裁决:不实施,理由见 `local_playground/bake/20260812/upgrade/07-GD设计条目裁决.md`。

### 沙箱内存快照（Freeze/Thaw）（候选,来源 GD-14-004）

重评触发(任一):L4 长驻会话(SandboxPersistent)出现有 metric 佐证的唤醒延迟问题;ADR-0104 的 Agent 层快照被证明不足以无损续跑(有具体失败案例)。2026-08-12 裁决:不实施——L2 为一次性执行,问题域仅 L4 且无实测数据。

### 节点级 Time-Travel 状态分叉重放（候选,来源 2026-08-13 GD-14-002）

现状:`035_task_checkpoints.sql` 主键 (task_id, node_id, attempt),覆盖崩溃恢复与 handoff 挂起恢复(ADR-0104),无 step_sequence / parent_checkpoint_id,不支持「倒带到第 N 步并改参分叉重跑」。

前置条件(缺一不可):
1. 实测案例:复杂多步任务中途偏离,现有粒度 + 恢复上下文确实无法续跑,只能整体重跑(附任务与 token 消耗数据)。
2. 分叉后的 Saga 补偿语义已定义(现有实现拒绝 MaxVisits>1 与 Compensation 同声明,见 `internal/execute/orchestrator/pattern_state_graph.go`;分叉的补偿逆序更复杂)。
3. Tier-0 实测每轮增量 state delta 的 SQLite 体积与写放大。

2026-08-13 裁决:不实施,属新功能非缺陷。

### 提示词 Layer 1 写入接口（候选,来源 C-1）

`internal/gateway/server/sysadmin/prompts.go` 的三个 handler 实现完整、`SysAdminHandler.PromptMgr` 已注入,但路由刻意未注册(2026-10 复核仍然如此;GR-9-003 曾误报为注入漏洞,实无生产入口)。

接线前置条件(缺一不可):
1. **审计**:写入/重置系统核心指令必须落 `audit_log`(HE-1)。
2. **注入校验**:`req.Value` 进 system prompt,需过 `internal/security/guard` 的 SystemPromptGuard。
3. **鉴权分级**:TierCritical 还是 TierAdmin,须与 killswitch loopback 豁免口径统一。

补齐后按 handler 注释接线:`GET/PUT/DELETE /v1/config/prompts/{name}`。

---

## 4. 工程纪律

### 4.1 七筛方法论（适用所有外部材料和技术提案）

1. **剥离比喻保留机制** — 神经科学/昆虫学/热力学比喻不入文档,只取可计算机制
2. **拒绝未验证算法引用** — 必须引用基准结果或可复现实验
3. **拒绝绝对化口号** — "0% CPU"/"零 token"/"零签名"不进设计文档
4. **拒绝魔数进设计文档** — 阈值参数化,由数据和 benchmark 决定
5. **可观测信号必须有可计算定义** — "智能"/"质量"/"异常"必须有具体公式
6. **MVP 保守,研究分支激进** — 好想法未过筛时进研究方向清单,不阻塞 MVP
7. **架构健壮性来自决议层稳定** — 决议层不动,只动参数和默认值

### 4.2 元层判断准则

- **架构原则比技术选择持久** — 决议层半年内不应被新技术冲击;只有参数与默认值会动
- **同一来源反复提议不构成重新引入理由** — 已被七筛筛掉的,重提仍拒
- **概念分解 ≠ 代码组织** — 13 模块对应 6 包是有意为之
- **可计算的小信号 > 不可计算的大概念** — SurpriseIndex 三组件具体,笼统指标不可用
- **EventLog 是真相源** — 任何派生引擎理论上可重建,须在测试中验证
- **横切关切独立成包** — M3/M11/M12 通过接口注入而非重复实现
- **物理隔离 > 提示词加固** — 安全防御只接受可"物理证伪"的机制(slot/sandbox/capability/audit)

### 4.3 完整反模式速查（跨模块共识,权威源）

控制流与 LLM 边界:
- 让 LLM 直接驱动控制流(违反不变量 6)
- 让 LLM 直接调工具(应由 Kernel 状态机分派)
- 重放时重新调 LLM(应记录值,确定性)
- 用 wall clock 做全序/退避/时间窗(NTP 漂移破坏)
- 自由 NL 多 agent 对话(60-66% 失败率,token 破产)

数据与持久化:
- 不可逆动作自动 rollback(物理上不可逆,只能告警 + 人工)
- 默认 forgetting = 物理删除(应默认归档,GDPR 例外)
- 跨引擎"同步事务"假象(不可实现,用 outbox + 最终一致)
- audit log 允许 DELETE/UPDATE(append-only 硬约束 + DB 触发器)
- 归档不物理删除撑满磁盘(M5 磁盘水位触发 Cold 数据淘汰)
- dead_letter 无具体处理流程(M2 §14.4 处理规范)

安全与边界:
- 抓取内容直接进 system prompt(必须 Taint=High 隔离到 data slot)
- 把 high-taint 直接拼进 instruction slot(物理 slot 分离破坏)
- 污点只升不降无受控降级路径(M11 Taint Graduation)
- builtin tool/系统密钥不走 capability(无后门原则)
- HTTP 默认绑定 0.0.0.0(单机优先,远程显式)
- 不可逆动作仅告警+人工不做预执行验证(M7 DryRunMode)

工程纪律:
- 价格表/魔数写进设计文档(参数化,由数据决定)
- 神经科学/昆虫学比喻泄露到代码层(只取机制,不取命名)
- 把概念模块和代码包混为一谈(13 概念 vs 6 包)
- SurpriseIndex Day 1 直接入熔断器(先有可计算定义,先旁路观测)
- 影子执行全量快照拖慢自学习闭环(M12 增量快照)
- 各模块后台 worker 无全局内存背压协调(M3 OSMemoryGuard 三级水位线统一管辖)

基础设施约束:
- 引入 Redis/Kafka/RabbitMQ(单机优先,违反硬约束)
- 用 cgroups 做单机 App 内存隔离(macOS/Windows 无此机制,进程内 Governor)
- 将图向量存储强制融合为单引擎(2026 无成熟嵌入式 Vector-Graph DBMS)
- 用 DID 替代 OIDC 做 MCP 认证(无成熟生产实现)
- 用"动态功能验证"替代结构化 JSON Schema(违反不变量 6)

---

## 5. 主动决议拒绝清单

> 反复被外部材料提议但本项目明确不采纳。同一来源反复提议不构成重新引入理由(七筛第 7 条)。

| 提议 | 拒绝理由 |
|------|---------|
| 神经科学比喻(LTP/LTD/突触可塑性) | 七筛第 1 条。已采纳机制本身(使用驱动的边权重 + 时间衰减),不引入命名 |
| 昆虫学比喻(Stigmergy/信息素) | 同上。已采纳"共享状态隐式协调"机制 |
| 调质浓度池 / Neuromodulator | 不可工程化,Policy 层 stress-mode 切换可替代 |
| 4K Token 工作记忆硬上限 | 七筛第 4 条魔数禁令 |
| 零签名自发现 MCP | 七筛第 3 条,与 Capability 冲突 |
| 0% CPU 空闲占用 | 同上,以"空载 CPU < 1%"可测量目标替代 |
| Inviolable(不可侵犯) | 同上,改为 High-Assurance,承诺"突破必留痕" |
| 15 模块拆解(独立 Metacognition/WorldModel/Personalization/UI) | 过度分解,功能已分配在 13 模块体系 |
| 5 大挽具层收敛 | 概念分解 ≠ 代码组织,13 概念 + 6 包共存有意为之 |
| DID 替代 OIDC 做 MCP 认证 | 无成熟生产实现 |
| 图向量统一引擎 | 2026 无成熟嵌入式 Vector-Graph DBMS |
| Schema 过时 / 动态功能验证替代 JSON Schema | 违反不变量 6 |
| 引入 Redis/Kafka/RabbitMQ | 违反单机硬约束 |
| cgroups 做单机 App 内存隔离 | macOS/Windows 无此机制,应用进程内 Governor |
| 已集成多模态 / M09 自主补全引擎 | M01/M13 已支持图片、视频分析与 TTS;M09 已实现缺失能力自主探测与合成 |
| A2A 入站发现端点 / Agent Card 缺失 | **已实现**:Agent Card(`/.well-known/agent-card.json`)+ A2A 1.0 入站 `/a2a/v1`(MCP 门面)见 ADR-0111;旧 `POST /v1/a2a/tasks` 仅在 `LegacyEndpoint` 开启时注册(`internal/gateway/server/server_routes.go`)。反复被报为「缺失」,均因未 grep 路由表 |
| 把生产启动迁移到自动装配编排（原 bootstrap.Bootstrapper） | `ARCHITECTURE.md §8.2` + `ADR-0088 §决策七`。该包已于 2026-10-13 删除（ADR-0112 决策八 #1）；重评触发：手工装配链 > 6000 行 |
| WASI 0.2 Component Model 完整支持 | `ADR-0008 §决策五`。配置开关已开、功能未落地;重评触发条件见该节 |

---

**END OF ROADMAP.md** — 与 [ARCHITECTURE.md](./ARCHITECTURE.md) 配套阅读。

## 已驳回方案（Why-Not 载荷）

| 方案 | 驳回时间 | 驳回理由摘要 | 所属 ADR |
|------|----------|------------|----------|
| GD-14-001 多语言 SDK 随主版同步发布 | 2026-08-12 | 工程与效益不匹配;现阶段只建设 Python SDK + REST | ADR-0091 GD 步骤 |
| GD-14-002 内置工具动态载入 | 2026-08-12 | 工具集合静态,内置动态不带来明显收益 | ADR-0091 GD 步骤 |
| GD-14-003 统一提供方护屏 | 2026-08-12 | 各 Provider 情境差异极大,统一护屏难以成立 | ADR-0091 GD 步骤 |
