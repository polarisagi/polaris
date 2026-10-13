# Polaris — 架构总览

> **一句话定位**：主入口 + SSoT 锚点。详细模块设计见 `M01..M13.md`，概念定义见 [00-Global-Dictionary](./00-Global-Dictionary.md)，文档导航见 [INDEX.md](./INDEX.md)。
>
> **代码位置**：根目录
>
<!-- §跳读: 0-bis:11 哲学 / 0:19 SSoT / 1:25 定位约束 / 2:70 (SOFT)部署 / 3:83 Staging / 4:109 HT0预算 / 5:138 变更控制 / 6:152 配置层 / 8:167 启动与关停协议 / 7:204 244 当前实现状态 -->

---

## 0-bis. 设计哲学（三句话）

1. **大模型是协处理器，不是指挥官**：Go 确定性状态机持有控制流，LLM 做概率性填空，不做自由编排。三角色 Swarm（Supervisor / Librarian / Governance）物理隔离，通过异步黑板协调。
2. **物理安全边界优先**：五级 Taint 只升不降 + Cedar deny-by-default + 三级沙箱。概率过滤不是安全边界（[HE-Rule-2]）。
3. **Eval 门控进化**：Prompt、Skill、Config、源码补丁全部走 Staging 七阶段 CI。MEMF 持久化失败记忆，幻觉改进无法晋升至生产记忆。

---

## 0. SSoT 四层

L1 架构散文（本文档 + 模块文档） → L2 结构真相（`internal/protocol/schema/*.sql` DDL） → L3 状态机规约（`spec/state.yaml`） → L4 行为真相（`internal/` + `internal/protocol/*.go` 编译器强制）。四层之间是引用关系，非复制关系。

---

## 1. 项目定位与硬约束

**定位**：面向 2026+ 的**开源自托管** AI Agent — Go + Rust 双语言，多 Agent 协同，自演化。主形态为桌面版（Tauri v2 外壳 + Go 守护进程 sidecar）+ 浏览器，同一守护进程亦可部署 VPS；HT0 8GB 为开发推荐地板（ADR-0096）。

**用户类型**（驱动可扩展性设计）：
- `[Operator-Developer]`：Fork 源码、参与贡献，可直接改 Go 代码。
- `[End-User]`：下载二进制/Docker 镜像自托管，通过 YAML 配置 + Web/桌面 UI 驱动，**不修改 Go 源码**。

**可扩展性契约**（End-User 的扩展边界；信任模型见 ADR-0103）：
- Skills（Wasm）：LLM 主动调用的能力扩展，End-User 可自行编写/加载
- Hooks：`hooks.json` 生命周期事件（SessionStart / UserPromptSubmit / PreToolUse / Stop 等），用户级 `<data>/hooks/hooks.json` 与插件 hooks（审阅信任后执行，ADR-0103 决策六）；见 `[ShellHooks]`（00-Dict §1）
- MCP 工具：外部工具接入，配置驱动
- 配置文件：`configs/*.yaml` 控制运行时行为

**设计目标**：
- 单机 HT0 8GB 完整运行；远程 LLM API 为主路径（provider-agnostic，`configs/defaults.toml` 默认推荐 DeepSeek V4 系列）；**本地推理默认关闭**，向量化默认进程内 ONNX 嵌入器，llama-server/Ollama 仅在用户显式配置时启用（ADR-0109）
- 多 Agent 黑板 + CAS 协调，禁止自由 NL 对话；三角色默认拓扑
- CodeAct：复杂任务编译为可验证沙箱代码（M07 §7.4）
- Test-Time Compute：SurpriseIndex 驱动 System 1 / 1.5 / 2 三档路由（00-Dict §1-ter XR-02 + M04 §5）
- 自学习自进化，所有变更经 staging 7 阶段闸控（§3）；MEMF 沉淀失败知识
- 长时高质运行，token 使用必须有可计算价值：TokenBurnRate 一等公民，cache-first 前缀 + 用量账本（ADR-0105）
- 会话归属项目（Project）容器，绑定工作目录，记忆按项目隔离（ADR-0097）
- 多台独立节点可经 MCP 门面 / A2A 1.0 互相委派任务（ADR-0111）

**非目标**：分布式/集群 | 单机端侧 7B+ 梯度训练 | 重型基础设施（Kafka/Redis/K8s） | 暴露 0.0.0.0

> 「跨节点」非目标指**单个 Agent 跨机分布**；独立节点间的任务委派不在此列——每节点仍是单进程、单库、互不共享状态（ADR-0111）。远程绑定走 `interface.extra_listen` 显式地址，拒绝 0.0.0.0（除非 `allow_wildcard_listen`）；TLS 本期依赖 Tailscale 链路加密，已知偏差见 ADR-0111。

**硬约束**：
- 内存：HT0 8GB 主线，峰值 ≤ 7GB
- 语言：Go（I/O + 编排 + 服务）+ Rust（推理 + 存储 + 沙箱），不引第三种
- 进程：守护进程单进程主体，Rust 经 cdylib via purego，不引数据面 sidecar；桌面外壳（Tauri v2）把 Go 二进制作为 sidecar 分发，外壳不链接内核，客户端与内核之间只有 HTTP/SSE 一条路（ADR-0096）
- 存储：嵌入式优先（SQLite + SurrealDB-Core + 本地 FS），禁独立进程 DB
- 网络：默认 127.0.0.1，远程绑定需显式 + capability + audit
- 安全：物理隔离 > 提示词加固，外部内容 Taint=High 默认
- 语音：STT 强制 Sherpa-ONNX + SenseVoice；服务端 TTS 为 MeloTTS；按总内存分 A/B/C 三档（A 关、B 仅 STT、C STT+Melo，Melo 另受 RTF 基准门控，未启用时前端系统语音兜底）；资产后台串行预置、懒加载与空闲卸载。详见 ADR-0110

**权威源指引**：
- HE 六不变量：[00-Global-Dictionary §1-bis](./00-Global-Dictionary.md)；跨模块交互规则 XR-01~07：[§1-ter](./00-Global-Dictionary.md)；两维度执行模型：[§9-ter](./00-Global-Dictionary.md)
- 反模式 + 拒绝清单：[ROADMAP §4.3 / §5](./ROADMAP.md)
- 13 模块职责边界：各 [M01..M13](./) 文件 §0
- C4 视图 + 时序图：[DIAGRAMS.md](./DIAGRAMS.md)；ADR 索引：[decisions/README.md](./decisions/README.md)；文档拓扑 + 加载预算：[INDEX.md](./INDEX.md)

---

## 2. 部署视图 — Hardware Tier(SOFT)

| Tier | 内存 | 本地推理 | 默认路径 | 梯度训练 | 沙箱 |
|------|------|----------|----------|----------|------|
| HT0 | 8GB | 不预加载 | 远程 API（provider-agnostic）| 禁用 | L1 InProc + L2 Rust 沙箱 |
| HT1 | 16GB | 7-8B 可选 | 远程为主 | 1-3B QLoRA | + gVisor/VZ L3 |
| HT2 | 24GB+ | 14B reasoning 可选 | 远程为主 | 7B QLoRA / DPO | + Firecracker(Linux KVM) |
| HT3 | 64GB+ (Mac M) | 32B+ 可全本地 | local_only / 离线 | 全套 | 全套 |

主线在 HT0 完整运行（`configs/defaults.toml` tier=0）。HT1+ 能力由 `internal/observability/probe/feature_gate.go` + `internal/observability/auto_config.go` 启动时硬件探测自动解锁。「本地推理」列均需用户显式启用（ADR-0109）。`privacy_mode` 四档（`local_only`/`allowlist`/`cost_optimized`/`quality_optimized`）与 Tier 正交。详见 00-Dict §1 [Tier-X-Limit]；实施现状见 [ROADMAP §2](./ROADMAP.md)。

---

## 3. Staging 流水线 7 阶段(权威定义)

M9 自进化候选（`skill` / `lora` / `prompt` / `config` / `source_patch` / `user_preference`）遵循**基于爆炸半径的分层豁免**（Radius-based Rollout）。

```
Stage 1: candidate_emit       (M9 worker 产出 → staging 表)
Stage 2: schema_validate      (M11 静态校验 + signature 验证 + sensitive_pattern_filter)
Stage 3: initial_eval         (M12 黄金集 baseline vs candidate)
Stage 4: replay_shadow        (M12 历史 trajectory 重放对比；ShadowExecutor 见 M12 §4/§8)
Stage 5: mirror_shadow        (M12 实时影子流量副本,Evo-L3+；未实现——ADR-0025 §K 不采纳实时流量镜像,以异步 EventLog 回放即 Stage 4 替代)
Stage 6: canary_rollout       (M11 流量比例渐进升,HITL 可介入)
Stage 7: full_promotion       (写生产 + audit hash chain)
```

**分层豁免规则**：
- **L0 配置调整**（如路由权重）：Stage 1-3 → 直接 Stage 7。依靠 M3 Telemetry 即时回滚兜底
- **L1 Prompt / 启发式**：Stage 1-4 + 加速 Stage 6 + Stage 7
- **L2 新技能生成**（Python + ContainerSandbox，ADR-0008）：Stage 1-6 + Stage 7
- **L3 / L4 策略修改 / 架构源码**：强制完整 Stage 1-7

任一阶段失败 → `rejected` / `rolled_back` / `dead_letter`。safety case 一票否决。

> **与 M09 Rollout 映射**：Stage 6 canary_rollout 由 `internal/learning/optimizer/rollout.go` 展开为六阶段渐进发布（0→1%→5%→25%→100%→Committed，详见 M09 §2.3），Stage 7 对应 Committed 状态落库。两套编号不冲突。

---

## 4. HT0 全模块容量预算累加表

> HT0 全模块预算唯一权威源。各模块子组件细分见对应模块文档。

| 模块 | Remote (MB) | local_only (MB) | 备注 |
|------|-------------|------------------|------|
| M01 Inference | 60 | 2,060 | local_only 为显式启用本地生成模型的情形(Qwen3-3B ~2GB) |
| M02 Storage | 260 | 260 | SQLite(纯 Go)+ SurrealDB-Core |
| M03 Observability | ~40 | ~40 | OTel + Prometheus + slog(桌面;服务器 ~70MB) |
| M04 Agent Kernel | 110 | 110 | 3 Agent goroutine 栈 + DAG buffer |
| M05 Memory | 177 | 177 | Mem-L0..L3 四层 |
| M06 Skill Library | 65 | 65 | Wasm cache Gold/Silver/Bronze |
| M07 Tool Action | 90 | 90 | Rust 脚本沙箱 + MCP Client |
| M08 Orchestrator | 89 | 89 | Blackboard + Agent goroutines ×2/×3 |
| M09 Self-Improve | 30 (idle) | 30 (idle) | Worker pool suspend-on-idle |
| M10 Knowledge RAG | 155 | 155 | 文档树 + Chunk 向量 + GraphRAG |
| M11 Policy Safety | 28 | 28 | Cedar FFI + AuditTrail + SafeDialer cache |
| M12 Eval Harness | 20 | 20 | EvalStore + TrajectoryRecorder |
| M13 Interface | 46 | 46 | HTTP/WS + TaskQueue + SurrealDB-Core KV |
| **应用层合计** | **~1,220** | **~3,220** | — |
| OS 预留 (macOS) | 1,500 | 1,500 | Darwin kernel + WindowServer |
| OS 预留 (Linux) | 800 | 800 | headless |
| **总计 (macOS)** | **~2,720** | **~4,720** | 8GB 内 ✓ |
| **总计 (Linux)** | **~2,020** | **~4,020** | 8GB 内 ✓ |

峰值：`local_only` 下 macOS 可达 ~5.5GB（M9 PromptOptimizer + M10 GraphRAG + M7 并发 Wasm），仍低于 7GB 硬上限。M3 OSMemoryGuard L1 预警（1.5GB Free）触发后台任务降级（M9/M10），不影响用户交互。

---

## 5. 变更控制

任何架构变更须按以下顺序联动同步（缺一即为孤立修改，禁止合并）：

1. **ADR**：全局/宪法级 → 新建 `decisions/ADR-NNNN-*.md`；模块级 → 对应模块 "## 决策" 节
2. **DDL**：数据模型 → `internal/protocol/schema/`
3. **接口**：跨模块契约 → `internal/protocol/interfaces_*.go`（按域拆分）/ `types.go`
4. **状态机**：状态/不变量 → `spec/state.yaml`
5. **概念标签**：标签语义 → `00-Global-Dictionary.md`

只改其中一层（如只改 DDL 不改接口和模块文档，或只改代码不更新 state.yaml）即孤立修改；修改宪法级内容必须留 ADR。

---

## 6. 配置层与热加载协议

四层优先级（高覆盖低）：

```
Default 代码常量 < ~/.polaris/config/m*.toml（或 POLARIS_THRESHOLDS_DIR）< 环境变量(POLARIS_*) < CLI 启动参数
```

1. **加载与验证边界**：所有配置在进程启动期由 `internal/config` 统一装载与反序列化（含 `data_dir`/`host`/`port`，并据此预建运行子目录）。校验缺失或格式错误 Fail-Fast，不允许在 Agent 执行期延迟崩溃。Threshold 在 dataDir 解析后经 `config.LoadThresholds(dataDir)` 单独加载，避免 dataDir 未知时的 chicken-and-egg。
2. **热更新约束**：
   - 通用运行参数（日志级别、调度池上限等）→ 启动时装载；热更新路径（fsnotify + atomic.Value）已设计，待 `internal/config` 实现 `WatchReload`。
   - 核心防线（Cedar Policy、KillSwitch 门限、M2 Storage 路径等）的 `ZoneImmutable` 常量启动后冻结，禁运行期修改，须重启进程生效（Crash-Only 哲学）。

---

## 8. 启动与关停协议（Bootstrap & Graceful Shutdown）

> 启动序/退出序的架构落点（DS-10-005）。改启动依赖或关停清扫职责前必读——顺序错一步的典型后果是循环依赖起不来，或关停时子进程变孤儿、WAL 没落盘。

### 8.1 生产启动序：`cmd/polaris/` 手工分阶段装配

实际跑的是 `cmd/polaris/main.go run()` 里一条**显式线性装配链**，不是自动拓扑排序。每阶段一个 `boot_*.go`，产出 Bundle 供下游消费，层级严格 L0 → L3 下沉：

| 序 | 入口 | 产出 | 承载 |
|---|---|---|---|
| 1 | `bootSubstrate` | `SubstrateBundle` | L0：配置加载、DataLayout、Store（SQLite/SurrealDB）、KillSwitch、OutboxWorker、审计链、Dialer |
| 2 | `bootMemory` | `MemoryBundle` | 四层记忆 + MEMF + Consolidation Worker |
| 3 | `bootTools` | `ToolBundle` | 工具注册（4 个注册入口，见 M07 §3.1）、沙箱、L4 长驻会话池 |
| 4 | `bootKnowledge` | `KnowledgeBundle` | RAG + GraphRAG + Connector |
| 5 | `bootAgent` | `AgentBundle` | Agent Kernel、AgentPool、M09 自进化、Supervisor Tree、Reaper |
| 6 | `bootServer` | `*server.Server` | L3：HTTP/SSE 网关、Channel 适配器、各类 setter 注入 |

**依赖方向由参数签名物理保证**：`bootTools(ctx, sb, mb)` 拿不到 `kb`/`ab`，编译期杜绝倒挂引用——相比运行期拓扑排序的唯一实质优势。跨节点委派、嵌入器选择等附加装配在同目录 `boot_delegation.go` / `boot_embedding.go` / `boot_mesh.go`，依旧受上述分层约束。

**关停序**：`<-ctx.Done()`（`signal.NotifyContext` 捕获 SIGINT/SIGTERM）后走 30s 超时清扫链——`httpSrv.Shutdown`（停流）→ `ab.ReaperStop`（须早于 dbWriter 排空）→ `AuditChain.VerifyChain`（审计链完整性）→ 其余经 `defer` 按 LIFO 逆序释放（`tb.PersistentSandbox.Shutdown` 终止 L4 解释器子进程防孤儿、`sb.Store.Close`、`sb.LogFile.Close`）。另有 `TripleCtrlCGuard`：连续 SIGINT 触发 `KillSwitch.OnSIGINT`。

### 8.2 已删除的自动编排契约（bootstrap）

bootstrap 包（`Bootable` + `DependencyMap` + Kahn 拓扑 + 四阶关停）与 `internal/swarm` 的 `PhasedStartup` 两套启动编排因生产零接线，已于 2026-10-13 删除（ADR-0112 决策八 #1，ADR-0088 决策七）。生产装配唯一落点是 `cmd/polaris/boot_*.go`（§8.1）。

> 重评触发：手工装配链 > 6000 行时重新评估装配方案；删除前的实现可从 git 历史取回。与 `Module-Dependency-Axioms.md §2.4` 不冲突：该节 [MUST]「注入必须且仅能在装配层完成」约束职责归属，装配层即 `cmd/polaris/boot_*.go`。

### 8.3 `internal/cli/`：已于 2026-09-21 删除

该包曾是未接线的 CLI 引导契约（`AgentREPL` / `RateLimiterMiddleware` / `WebSocketHub`），按 ADR-0096 决策六删除：`AgentREPL` 依赖 `InferFn` 直连内核，与"客户端与内核之间只有 HTTP/SSE 一条路"正面冲突。命令行版实现在 `cmd/polaris/cli*.go`（HTTP 客户端，不直连数据库与内核）；服务发现所需的运行时状态读写在 `internal/runtimeinfo`。

---

**END OF ARCHITECTURE.md**

---

## 7. 当前实现状态（代码与文档对齐）

> 状态以代码实际行为为准。2026-10 复核：M1–M13-bis 全部落码；近期增量以 ADR-0096/0097/0098/0103/0104/0105/0109/0110/0111 为准，模块文档已随之更新。

### 7.1 模块整体完成度

M1–M13-bis 均为 ✅ 完整，下列为需知的设计取舍/边界：

| 模块 | 说明 |
|------|------|
| M3 Observability | SurpriseIndex 基础版（两组件简化版）；完整版在 M9 |
| M7 Tool Sandbox / MCP | sandbox 内部不重复 Policy 校验（设计决策，依赖 ExecuteTool 前置 PolicyGate） |
| M9 Self-Improvement | 见下方 [M9 补充](#m9-补充) |
| M11 Policy / Safety | `polaris_cedar_degraded_total` Counter 已实现（定义于 `internal/observability/metrics/metrics.go`，埋点于 `internal/security/policy/gate.go`） |
| M13 Scheduler / Gateway | 鉴权依赖部署配置（令牌/`POLARIS_API_KEY`）；admin 写入限 localhost |
| M13-bis Extension Registry | `marketplace.NewManager` 在 `cmd/polaris/boot_tools.go` 直接装配，无独立 `WithRegistrar` 注入点 |

#### M9 补充

- L2 LogicCollapse 编译后提交 StagingPipeline（`SubmitCandidate`）已实现。
- `internal/automation/hitl/gateway.go` 的 HITLGateway 含强制冷却 + 全量回归测试 + 影子报告；仅 `CheckpointType == "l4_multi_sig"`（`internal/learning/engine.go` `detectL4Trigger`，L4 自我改进候选晋升）触发。
- `LightweightRegressionDetector.DetectRegression`（`internal/eval/regression/detector.go`）基于 EventLog 近期/基线窗口 type 分布 + 错误类 topic 占比对比。
- 明确不做多签（单用户场景无意义）。

### 7.2 已落码的跨模块重构（原"已知缺口"，均已闭环）

- **M13 ChatOrchestrator 拆分**（ADR-0066 目标态，含原 ADR-0085）：`internal/gateway/session` 收敛 SSE 与 Headless 两条入口的会话生命周期编排（会话确保/历史加载/Hook 分发/斜线命令路由/压缩决策/`SystemPromptGuard` 装配/消息持久化），`chat/sse.go` 仅保留请求解码/SSE 帧翻译/错误码映射。零 `net/http` 依赖 + 唯一 FSM 轮次入口两条防退化 lint 已接入 CI。
- **跨 Agent / 跨节点委派**（ADR-0111，含原 ADR-0084、ADR-0017 决策三）：出站外部 Agent 建模为 MCP 工具类别（`mcp:<server>/<agent>` 寻址），复用 `transfer_to_agent` 异步挂起机制 + `MCPManager.CallTool`；入站为 `/mcp`（Polaris 对端）与 `/a2a/v1`（外部 A2A 1.0）两个门面，共用一个内部委派服务。

### 7.3 审计依据

2026-06-15 全量核查，其后逐 ADR 增量更新；2026-10-12 文档优化轮次对总览章节做代码对齐。
