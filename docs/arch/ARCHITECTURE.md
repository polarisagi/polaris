# Polaris — 架构总览

> 主入口 + SSoT 锚点。详细模块设计见 `M01..M13.md`,概念定义见 [00-Global-Dictionary](./00-Global-Dictionary.md),文档导航见 [INDEX.md](./INDEX.md)。
> **§跳读**: 0:8 SSoT / 1:16 定位约束 / 2:62 49(SOFT)部署 / 3:75 Staging / 4:101 HT0预算 / 5:130 变更控制 / 6:146 配置层

---

## 0. SSoT 四层

L1 架构散文(本文档+模块文档) → L2 结构真相(`internal/protocol/schema/*.sql` DDL) → L3 状态机规约(`spec/state.yaml`) → L4 行为真相(`pkg/` + `internal/protocol/*.go` 编译器强制)。

四层之间是引用关系,非复制关系。

---

## 1. 项目定位与硬约束

**定位**: 面向 2026+ 的**开源自托管** AI Agent — 单机 8GB+ 内存可运行,Go + Rust 双语言,多 Agent 协同,自演化。

**用户类型**（驱动可扩展性设计决策）:
- `[Operator-Developer]`: Fork 源码、参与贡献的技术用户。可直接修改 Go 代码。
- `[End-User]`: 下载二进制/Docker 镜像自托管的普通用户。通过 YAML 配置 + Web UI 驱动，**不修改 Go 源码**。

**可扩展性契约**（开源后 End-User 的扩展边界）:
- Skills（Wasm）: LLM 主动调用的能力扩展，End-User 可自行编写/加载
- Shell Script Hooks: 生命周期事件（`gateway.startup` / `session.new` / `message.after` 等）自动触发用户脚本，零依赖、任意语言（`~/.polarisagi/polaris/hooks/`）
- MCP 工具: 外部工具接入，配置驱动
- 配置文件: `configs/*.yaml` 控制所有运行时行为

> Shell Script Hooks 为 End-User 级生命周期扩展能力（参见 `[ShellHooks]` in 00-Global-Dictionary §1）。Operator-Developer 直接改源码，无需 hooks。

**设计目标**:
- 单机 HT0 8GB 完整运行
- 远程 LLM API 主路径（provider-agnostic；`configs/defaults.toml` 默认推荐 DeepSeek V4 系列），本地推理为隐私/离线 fallback
- 多 Agent 黑板 + CAS 协调,禁止自由 NL 对话
- 自学习自进化,所有变更经 staging 7 阶段闸控(§3)
- 长时高质运行,token 使用必须有可计算价值
- End-User 无需修改源码即可自定义生命周期行为

**非目标**: 分布式/跨节点/集群 | 单机端侧 7B+ 梯度训练 | 重型基础设施(Kafka/Redis/K8s) | 暴露 0.0.0.0

**硬约束**:
- 内存: HT0 8GB 主线,峰值 ≤ 7GB
- 语言: Go(I/O + 编排 + 服务)+ Rust(推理 + 存储 + 沙箱),不引第三种
- 进程: 单进程主体,Rust 经 cdylib via purego,不引 sidecar
- 存储: 嵌入式优先(SQLite + SurrealDB-Core + 本地 FS),禁独立进程 DB
- 网络: 默认 127.0.0.1,远程绑定需显式 + TLS + capability + audit
- 安全: 物理隔离 > 提示词加固,外部内容 Taint=High 默认
- 语音: 本地 STT 选型强制为 Sherpa-ONNX + SenseVoice（零 Python 依赖，极低内存开销），严格对齐 Tier 0 约束。

**权威源指引**:
- HE 六不变量(可观测/可验证/可组合/数据驱动/状态机控制流/State-in-DB): [00-Global-Dictionary §1-bis](./00-Global-Dictionary.md)
- 跨模块交互规则 XR-01~07: [00-Global-Dictionary §1-ter](./00-Global-Dictionary.md)
- 反模式完整清单 + 拒绝清单: [ROADMAP §4.3 / §5](./ROADMAP.md)
- 两维度执行模型(任务难度 × 推理深度): [00-Global-Dictionary §9-ter](./00-Global-Dictionary.md)
- 13 模块职责边界: 各 [M01..M13](./) 文件 §0
- C4 视图 + 关键流程时序图: [DIAGRAMS.md](./DIAGRAMS.md)
- ADR 索引: [decisions/README.md](./decisions/README.md)
- 文档拓扑 + 加载预算: [INDEX.md](./INDEX.md)

---

## 2. 部署视图 — Hardware Tier(SOFT)

| Tier | 内存 | 本地推理 | 默认路径 | 梯度训练 | 沙箱 |
|------|------|----------|----------|----------|------|
| HT0 | 8GB | 不预加载 | 远程 API（provider-agnostic）| 禁用 | L1 InProc + L2 Rust 沙箱 |
| HT1 | 16GB | 7-8B 可选 | 远程为主 | 1-3B QLoRA | + gVisor/VZ L3 |
| HT2 | 24GB+ | 14B reasoning 可选 | 远程为主 | 7B QLoRA / DPO | + Firecracker(Linux KVM) |
| HT3 | 64GB+ (Mac M) | 32B+ 可全本地 | local_only / 离线 | 全套 | 全套 |

主线在 HT0 完整运行（`configs/defaults.toml` tier=0）。HT1+ 能力由 `pkg/substrate/observability/feature_gate.go` + `auto_config.go` 启动时硬件探测自动解锁，无需代码修改。`privacy_mode` 四档(`local_only`/`allowlist`/`cost_optimized`/`quality_optimized`)与 Tier 正交。详见 00-Dict §1 [Tier-X-Limit]。实施现状（哪些 Tier 已验证 / 待硬件）见 [ROADMAP §2 工程现状](./ROADMAP.md)。

---

## 3. Staging 流水线 7 阶段(权威定义)

M9 自进化候选(`skill` / `lora` / `prompt` / `config` / `source_patch` / `user_preference`)遵循 **基于爆炸半径的分层豁免**(Radius-based Rollout)机制。

```
Stage 1: candidate_emit       (M9 worker 产出 → staging 表)
Stage 2: schema_validate      (M11 静态校验 + signature 验证 + sensitive_pattern_filter)
Stage 3: initial_eval         (M12 黄金集 baseline vs candidate)
Stage 4: replay_shadow        (M12 历史 trajectory 重放对比)
Stage 5: mirror_shadow        (M12 实时影子流量副本,Evo-L3+)
Stage 6: canary_rollout       (M11 流量比例渐进升,HITL 可介入)
Stage 7: full_promotion       (写生产 + audit hash chain)
```

**分层豁免规则**:
- **L0 配置调整**(如路由权重): Stage 1-3 → 直接 Stage 7。依靠 M3 Telemetry 即时回滚兜底
- **L1 Prompt / 启发式**: Stage 1-4 + 加速 Stage 6 + Stage 7
- **L2 新技能生成**(Wasm): Stage 1-6 + Stage 7
- **L3 / L4 策略修改 / 架构源码**: 强制完整 Stage 1-7

任一阶段失败 → `rejected` / `rolled_back` / `dead_letter`。safety case 一票否决。

> **与 M09 Gate 编号映射**: M09 §2.3 使用 Gate 0-4 标注外环阶段（Gate 0=Eval离线回归 / Gate 1=Shadow 1% / Gate 2=Shadow观测 / Gate 3=Canary / Gate 4=Full Rollout），与本节 Stage 1-7 是两套语言体系。Stage 1-7 是全局流水线阶段编号，Gate 0-4 是 M09 外环推进监测的实现细化。L2+ 候选的 Stage 3-6 对应 M09 Gate 0-3。

---

## 4. HT0 全模块容量预算累加表

> HT0 全模块预算唯一权威源。各模块子组件细分见对应模块文档实现章节。

| 模块 | Remote (MB) | local_only (MB) | 备注 |
|------|-------------|------------------|------|
| M01 Inference | 60 | 2,060 | local_only 加载 Qwen3-3B (~2GB) |
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

峰值: local_only macOS 可达 ~5.5GB(M9 PromptOptimizer + M10 GraphRAG + M7 并发 Wasm),仍 < 7GB 硬上限。M3 OSMemoryGuard L1 预警(1.5GB free)在峰值时触发后台任务降级(M9/M10),不影响交互。

---

## 5. 变更控制

任何架构变更须按以下顺序联动同步(缺一即视为孤立修改，禁止合并):

1. **ADR**: 全局/宪法级 → 新建 `decisions/ADR-NNNN-*.md`;模块级 → 对应模块 "## 决策" 节
2. **DDL**: 数据模型 → `internal/protocol/schema/`
3. **接口**: 跨模块契约 → `internal/protocol/interfaces.go` / `types.go`
4. **状态机**: 状态/不变量 → `spec/state.yaml`
5. **概念标签**: 标签语义 → `00-Global-Dictionary.md`

判断标准：单独修改上述任一层而其他四层内容未同步就是孤立修改。例如：只改了 DDL 不改接口和模块文档，或只改了代码不更新 state.yaml——均属孤立修改。

不允许:孤立修改 spec / DDL / 接口 / 代码而不更新架构文档;修改宪法级内容而不留 ADR。

---

## 6. 配置层与热加载协议

四层优先级(高优先级覆盖低优先级):

```
Default 代码常量 < ~/.polarisagi/polaris/config/m*.toml（或 POLARIS_THRESHOLDS_DIR）< 环境变量(POLARIS_*) < CLI 启动参数
```

1. **加载与验证边界**: 所有配置必须在进程启动期由 `internal/config` 统一装载与反序列化（包括统一管理 `data_dir`/`host`/`port` 等基础环境，并据此在启动早期预建所有必需的运行子目录），校验缺失或格式错误引发 Fail-Fast，绝不允许在 Agent 执行期延迟崩溃。Threshold 加载在 dataDir 解析之后通过 config.LoadThresholds(dataDir) 单独进行，不在 config.Load() 内完成，避免 dataDir 未知时的 chicken-and-egg 问题。
2. **热更新约束**:
   - 通用运行参数（日志级别、调度池上限等）→ 启动时统一装载；热更新路径（fsnotify + atomic.Value）已设计，在 `internal/config` 实现 `WatchReload` 能力后可在不重启进程的情况下生效
   - 核心防线(Cedar Policy、KillSwitch 门限、M2 Storage 路径等)的 `ZoneImmutable` 常量 → 启动后冻结,禁运行期修改,必须重启进程使之生效(Crash-Only 哲学)

---

**END OF ARCHITECTURE.md**

---

## 7. 当前实现状态（代码与文档对齐）

> 基于 2026-06-11 代码审计。状态以代码实际行为为准，不以文档预期为准。

### 7.1 模块整体完成度

| 模块 | 文档覆盖率 | 代码完成度 | 主要缺口 |
|------|-----------|-----------|---------|
| M1 Provider Router | 完整 | ✅ 完整 | — |
| M2 Storage / EventLog / MutationBus | 完整 | ✅ 完整 | — |
| M3 Observability | 完整 | ✅ 完整 | SurpriseIndex 基础版（两组件简化版）已实现 |
| M4 Agent Kernel | 完整 | ✅ 完整 | ActiveContext.TaintLevel 跨轮次传播待实现；S_SUSPEND 状态缺失 |
| M5 Memory | 完整 | ✅ 完整 | — |
| M6 Skill Library | 完整 | ✅ 完整 | — |
| M7 Tool Sandbox / MCP | 完整 | ✅ 完整 | sandbox 路径 Policy 检查与 CallTool 路径不对等（P2） |
| M8 Multi-Agent Orchestrator | 完整 | ⚠️ 部分 | inv_M8_02 双写未实现；动态提权/Phased Startup/委托链校验缺失；内存版 CAS 校验 bug |
| M9 Self-Improvement Engine | 完整 | ⚠️ 部分 | 内环成功轨迹未写；L2/L3/L4 进化路径计划中；CurriculumGenerator 接口不匹配 |
| M10 Knowledge RAG | 完整 | ⚠️ 部分 | 结构化解析器缺失；摘要生成缺失；IncrementalIndexer 缺失；StructuredNavigator/QueryPlanner 缺失 |
| M11 Policy / Safety | 完整 | ✅ 完整 | Cedar FFI 静默降级至 Go 规则引擎时无可观测性警告 |
| M12 Eval Harness | 完整 | ✅ 完整 | — |
| M13 Scheduler / Gateway | 完整 | ✅ 完整 | 日志 SSE 端点在无 API Key 时对局域网开放（P2） |
| M13-bis Extension Registry | 完整 | ⚠️ 部分 | InstallExtension 为空壳；Automation/Agent 安装流绕过 Manager |

### 7.2 跨模块已知缺口汇总

| 缺口 | 严重度 | 涉及模块 | 说明 |
|------|--------|---------|------|
| Blackboard → EventLog 双写（inv_M8_02） | P0 | M8 | SQLiteBlackboard 直写 tasks 表，EventLog 无写入，崩溃后无法回放重建 |
| InstallExtension 安装流空壳 | P0 | M13-bis | Manager.InstallExtension 仅 PolicyGate，不写库不下载不注册 |
| Blackboard CompleteTask 阻塞写 channel | P0 | M8 | 内存版阻塞式 events <- 与其他非阻塞方法不一致，高并发下死锁风险 |
| SideEffectPreCheck Version 校验缺失 | P1 | M8 | 内存版 claimedVersion 入参完全未使用（ABA 漏洞） |
| 内环成功轨迹未写 HeuristicsMemory | P1 | M9 | Engine.Run() 仅处理失败事件，success_rate 永远为零 |
| 知识摄入 6 阶段严重缩水 | P1 | M10 | 无结构化解析器，无多级摘要，无 Embedding，无 ANN 索引 |
| Outbox RegisterOutboxHandlers 缺失 | P1 | M10 | GraphBuildWorker 无法被 Outbox 驱动 |
| ActiveContext.TaintLevel 未实现 | P1 | M4 | 跨轮次污点传播历史丢失，后续 DAG 可能绕过 TaintGate |
| SupervisorEpoch 非原子递增 | P1 | M8 | epoch++ 并发不安全 |
| Phased Startup P0→P4 未实现 | P1 | M8 | 启动时无策略真空窗口保护 |
| 委托链深度校验缺失 | P1 | M8 | SpawnDepth 字段存在但 PostTask 时未校验 |
| StructuredNavigator + QueryPlanner 缺失 | P1 | M10 | 三阶段结构化检索仅剩 Hybrid 内容检索路径 |
| IncrementalIndexer 缺失 | P1 | M10 | 无 Hash 对比增量检测，无 Tombstone + Compaction |

### 7.3 审计依据

代码审计日期：2026-06-11。审计报告见 outputs/ 目录：
- `audit_extensions_swarm.md`（pkg/extensions + pkg/swarm）
- `audit_substrate_cognition.md`（pkg/substrate + pkg/cognition）
- `audit_gateway_governance.md`（pkg/gateway + pkg/edge + pkg/governance）
