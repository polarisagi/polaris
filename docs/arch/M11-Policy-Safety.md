# 模块 11: Policy & Safety

> Go + Rust(Cedar CGO-Free FFI (purego)) | [Module-Topology] L0 | [Code-Package-Mapping] pkg/substrate/
> 设计约束: 三层宪法 + Taint Tracking 主防线 + Cedar 策略引擎 + KillSwitch | [HE-Rule-2] 可验证执行
> 更新日期: 2026-04-30
> **§跳读**: 0:10 职责 / 0-ter:47 不变量速查 / 1:60 三层宪法 / 2:86 Taint / 3:223 Cedar / 4:289 KillSwitch / 5:355 隐私 / 6:442 SSRF / 6.5:446 Factuality / 7:488 审计 / 8:512 多Agent宪法 / 9:539 威胁监控 / 13:553 降级 / 14:585 跨模块契约

---

## 0. 职责边界

| M11 **是** | M11 **不是** |
|-----------|-------------|
| 策略评估引擎（allow/deny/redact） | 业务逻辑实现者 |
| Capability Token 签发与撤销中心（短寿命 Ed25519） | 长期凭据持有者 |
| 沙箱分级规则定义（Sbx-L1/L2/L3 判定依据: TrustTier + RiskLevel） | 沙箱执行器（那是 M7 wazero/gVisor），沙箱选型由 M7 ToolRegistry 指定 |
| 污点标签传播规则（Taint 5 级 + PropagateTaint） | 污点数据存储者（那是 M2 events/chunks 表 TaintLevel 列） |
| 安全事件审计源（内存 HashChain 防篡改 + DB 永久化存档） | 通用业务日志（那是 M3） |
| KillSwitch 阶段变迁的唯一触发者 | KillSwitch 响应执行（M4/M8/M13 各自响应） |
| SafeDialer 统一网络出口（DNS + CIDR + TOCTOU） | 具体网络协议实现（那是 Go 标准库 net.Dialer） |

M11 与 M3/M12 的分工:
- **M3** 看到一切发生（原始事实）
- **M12** 评判做得对不对（质量）
- **M11** 决定能不能做（权限/安全）
三者事件流互通（都写 EventLog，topic 不同），但互不替代。

### 五条防线（纵深防御）

安全是**物理断裂，不是过滤器**。每条防线独立成立——任一条失效其余仍能阻挡。

| # | 防线 | 机制 | 守护对象 | 物理锚点 |
|---|------|------|---------|---------|
| **D1** | 数据污点追踪 | Taint 5 级 + Slot 物理分离 | 输入 | `pkg/substrate/taint.go` |
| **D2** | 能力令牌 | 短寿命 Ed25519 + 最小权限 + 委托链 ≤3 | 权限 | `pkg/action/capability_token.go` |
| **D3** | 沙箱分级 | Sbx-L1(InProc) / L2(Rust 脚本沙箱) / L3(平台原生 microVM: Linux Firecracker / macOS VZ / Windows WSL2，gVisor 仅作 Linux KVM 不可用 fallback) | 执行 | `pkg/action/sandbox.go` |
| **D4** | 宪法分层 | Layer 1(编译期常量) / 2(Cedar forbid) / 3(Cedar permit) / 4(多 Agent) | 决策 | `pkg/substrate/policy/` |
| **D5** | Kill Switch + Audit | 三阶段 FSM + hash chain 仅追加 | 系统 | `pkg/substrate/killswitch.go` |
| **D6** | [FactualityGuard] | 引用核验 + 数值一致性 + 抽样 LLM-as-Judge | **输出真实性** | `pkg/substrate/policy/` |

核心原则：**拒绝绝对化**——不承诺零突破，承诺"突破必留痕"（audit hash chain 不可篡改）。

**D1~D5 守护输入与权限边界；D6 (inv_global_06) 与 PII Guard 并列守护输出边界——LLM 输出的事实性。** 详见 §6.5。

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M11_01 | 五条防线独立生效——任一条失效其余仍能阻挡（纵深防御） | 安全审计逐条验证 |
| inv_M11_02 | TaintLevel 只升不降——output = max(inputs)，受控降级仅四种 Sanitizer | CI `taint_propagation` 测试 |
| inv_M11_03 | Audit Trail 仅追加不可篡改——hash chain + DDL 触发器禁止 UPDATE/DELETE | M11 §7.1 VerifyIntegrity |
| inv_M11_04 | KillSwitch FULLSTOP 200ms 内停止所有 tool call——`.fullstop` 持久文件防守护进程重启循环 | M11 §4.1 executeFullStop |
| inv_M11_05 | 所有出站连接经 SafeDialer.DialContext 五阶段 SSRF 防护——HTTP/3 QUIC 禁用 | CI `safe_dialer_lint` |
| inv_M11_06 | 拒绝绝对化——不承诺零突破，承诺"突破必留痕"（audit hash chain） | M11 ADR-010 |

---

## 1. 三层宪法架构

Layer 1 不可侵犯条款 — 编译期常量:
  AuditLogAlwaysOn = true
  KillSwitchEndpoint = "/_admin/kill"
  SelfModificationGuard = true
  TestInviolableConstants 编译期验证: 上述常量若移除或置 false → 编译/测试失败

**L4 运行时启动期保护**: M11 进程启动后第一时间计算核心安全相关源码目录及文件（包括 M4 Agent Kernel 的 pkg/cognition/kernel/、pkg/substrate/policy/、pkg/action/sandbox/、internal/config/immutable_constants.go、internal/protocol/schema/ 等）的当前 SHA-256 哈希集合，并与构建时通过 ldflags 注入（或 go:embed 静态绑定）在二进制文件内的权威 manifest 进行比对。若哈希值不一致（表明运行时源码已被篡改），则触发 fail-closed 拒绝启动并生成 CRITICAL 级别的审计日志。internal/protocol/interfaces.go 定义安全关键接口（SafeDialer/Cedar-Gate/TaintLevel/Blackboard），schema/ 定义 EventLog DDL——两者是安全链的契约基础，篡改即整个安全模型失效。
额外防御: 禁止 pkg/swarm/ 路径下使用 unsafe / reflect.Value.UnsafePointer / CGO（CI lint 强制）；进程启动期 SetGCPercent + SetMemoryLimit 锁定 runtime 参数，禁止运行时通过 reflect 修改。关键二进制可选 TPM/Secure Enclave 锚定（Tier 2+ 选配）。

Layer 2 硬约束 — Cedar forbid (无条件优先 permit):
  forbid: 不可逆操作未经审批 → resource.tool_name in [deploy_to_production, delete_data, send_external_communication, financial_transaction] AND context.approval_status != "approved"
  forbid: LLM 生成代码执行特权操作 → principal in Role::"Agent" AND resource.source == "llm_generated" AND resource.risk_level == "privileged"
  forbid: 预算硬上限 → context.monthly_spend_usd > context.monthly_budget_usd (所有 principal/action/resource 无条件)
  forbid: Holdout Set 读取隔离 → principal in Role::"Agent" AND action in [Action::"read_local", Action::"read_file"] AND resource.path.startsWith(context.polarisagi/polaris_eval_holdout_path)
    说明: context.polarisagi/polaris_eval_holdout_path 由 Go 侧在策略加载时注入（展开后的绝对路径，等价于 ~/.polarisagi/polaris/eval/holdout/）。ci_gate role 不受此 forbid 限制（CI/Canary 需要读取 Holdout Set）。此规则为防御纵深——物理隔离层（WASI 沙箱 + Openat2 RESOLVE_IN_ROOT）已阻止逃逸，Cedar 规则覆盖 Host Function 层可能的访问向量。

Layer 3 软约束 — Cedar permit + conditions:
  permit: 只读工具 → trust_level >= 1
  permit: 本地写入 → trust_level >= 2 AND resource.allowed_paths 包含 context.target_path
  permit: 网络操作 → trust_level >= 3 AND context.approval_status == "approved" AND context.capability_token_valid
  可热更新, 无需重启

---

## 2. Prompt Injection Defense — Taint Tracking 主防线

### 2.1 TaintedString / SafeString 类型系统 (四重防护)

TaintLevel/TaintedString/SafeString/TaintSource/PropagateTaint 类型定义见 `pkg/substrate/taint.go`。

四重防护说明：

| 防护层 | 机制 | 执行位置 |
|-------|------|---------|
| 第一重：编译期类型断裂 | `TaintedString` 与 `SafeString` 内容字段均不导出；`PromptBuilder.WriteInstruction` 仅接受 `SafeString`，`TaintedString` 无法直接进入 Instruction Slot | 编译期 Go 类型系统强制 |
| 第二重：CI 静态分析 | `go-vet taint_enforcement` 扫描：taint 包外构造裸 `SafeString`、包外访问 `TaintedString.content`、`string` 直传 `WriteInstruction` 均报 ERROR | CI 门控，PR 拒绝 |
| 第三重：持久化边界密码学验证 | SQL/JSON/Protobuf 序列化层附加 HMAC-SHA256（`persistent_key` OS Keychain 派生），反序列化时重算验证；验证失败或字段缺失 → 强制 `TaintHigh` + CRITICAL 审计；启动顺序 `CredentialVault.Init() → StorageFabric.Open()`，超时 30s 进程退出 | 运行时序列化钩子 |
| 第四重：泛型反序列化防剥离 | 禁止 `json.Unmarshal` 到 `map[string]interface{}` / `any` 等弱类型集合；外部 JSON 须反序列化到显式声明的带污点结构体 | CI go-vet lint 强制 |

### 2.2 辅助防线 (OWASP LLM Top 10 2025 防护)

执行顺序: Taint Gate → AnomalyDistanceFilter → Spotlighting → SIC Cleaning → SystemPromptGuard → Capability Gate

Spotlighting:
  步骤1: 生成标记 = SHA-256(content)[:8]（内容派生，非随机——保证重放确定性，M12 Eval 回放验证一致性）
  步骤2: "=== UNTRUSTED_DATA_{hex} ===\n{data}\n=== END_UNTRUSTED_DATA ==="
  调用方: M5 ContextAssembler.Build（ZoneTaintedData 追加前强制包裹）+ M4 上下文注入路径

SIC CleanInstructions (maxIter=5):
  步骤1 detect: LLM 检测 override/extract/reset/伪系统指令模式 → bool
  步骤2 rewrite: 替换为安全标记
  步骤3 iterate: 连续两次文本相同 → 完成
  步骤4 bailout: 达 maxIter 仍检测到 → ErrUncleanableContent

SystemPromptGuard (防系统提示词泄露 - OWASP LLM07):
  触发: 对向用户展示的终端回复以及所有 `write_network` 外部出站流量进行强制扫描。
  机制: 运行时使用 Aho-Corasick 算法比对出站文本与系统提示池，若发现连续重合 > 15 tokens 的片段，触发阻断。
  结果: 将重合片段擦除替换为 `[SYSTEM_REDACTED]` 或直接 ErrPromptLeakage 拒绝出站并记录审计。

AnomalyDistanceFilter (马氏距离异常检测 - OWASP LLM08):
  触发: LLM 接收外部非结构化请求之前。
  机制: 内存中维护各 `task_type` 历史特征向量的协方差逆矩阵。计算新输入向量的马氏距离，若 > 3σ（即极端偏离正常分布），判定为潜在模型越狱或毒化请求。
  结果: 阻断进入模型并打标 `TaintHigh`，转移至隔离区 (Quarantine) 并触发 M13 人工审计。

### 2.3 TaintLevel 五级量化 [TaintLevel]

| 级别 | 语义 | 来源 | Slot |
|------|------|------|------|
| TaintNone (0) | 系统编译期内容 | system slot / compiled prompts | instruction |
| TaintLow (1) | 用户原始输入 | instruction slot / workspace 内文件 | instruction |
| TaintMedium (2) | 受信第三方输出 | 白名单 MCP / allowlist connector | data (不可进 instruction) |
| TaintHigh (3) | 不受信外部内容 | 抓取网页 / 未知 MCP / shell stdout | data (不可进 instruction) |
| TaintUserReviewed (4) | 用户显式 review 后降级 | 用户确认操作 | data (不可进 instruction) |

### 2.4 传播规则 [Taint-Prop]

PropagateTaint: `output = max(所有输入的 TaintLevel)`, 只升不降

受控降级 Downgrade (仅以下路径):
  用户显式确认: high/medium → user_reviewed
  Schema 校验: high → medium (预定义 JSON Schema)
  确定性转换: high → medium (白名单纯函数)
  LLM 摘要: high → medium / medium → medium (TaintMedium 硬地板 [Taint-Floor-Medium])

自动降级绝对禁止。每次降级写入 audit_log。
受信源白名单初始 taint 最低 medium, 禁止 none/low。

**Connector Taint 初始等级查找表** (M10 §1.2 ConnectorScheduler 在调用 Ingester.Ingest 前显式打标):

| Connector | 初始 TaintLevel | 说明 |
|-----------|---------------|------|
| ObsidianConnector / LocalFolderConnector | TaintLow | 用户本地文件，用户拥有 |
| GitConnector（local file://） | TaintLow | 本地仓库 |
| GitConnector（remote allowlisted） | TaintMedium | 远程但在白名单内 |
| WebURLConnector / NotionConnector / GDriveConnector / DropboxConnector | TaintHigh | 远程第三方内容 |
| GmailConnector / SlackConnector / DiscordConnector | TaintHigh | 第三方通讯平台 |
| DBConnector | 用户配置时显式声明，默认 TaintHigh | — |

绝对禁止：Connector 来源初始 taint 设为 TaintLow（白名单内 connector 除外）；SanitizeBySummarization 将 TaintHigh 降至 TaintLow。CI lint 校验所有 Connector 实现都声明了 `InitialTaintLevel` 字段。

Taint 统计监控: [SurpriseIndex] gauge taint.high_ratio, 超阈值告警不自动降级。

### 2.5 污点清洗管道 [Taint-Sanitizer]

四种清洗方式:

SanitizeBySchema:
  条件: 数据通过预定义 JSON Schema 校验
  结果: data.Level = min(Level-1, TaintMedium)
  字符串字段附加约束: 仅当字段定义了 format / pattern / enum / const 时允许降级；裸 `{"type":"string"}` 不降级。数值/布尔/受限枚举字段不受此限制。
  嵌套结构: 任一深层子节点为无约束裸 string → 整个父结构不可整体降级（[Taint-Prop] max(inputs)）。
  审计: 每次降级写 audit_log，标注依据（format_guard / pattern_guard / enum_guard / const_guard / type_only）。

SanitizeBySummarization:
  条件: LLM 对 tainted 数据做摘要
  结果: data.Level = max(min(Level-1, TaintMedium), TaintMedium)
  硬地板: TaintMedium, 摘要永不进入 instruction slot
  M4 Layer A 豁免: 系统自生成摘要 (source='compaction'/'persona_refinement'/'consolidation'/'skill_compilation') 标记 TaintMedium 但不参与 ActiveContext.TaintLevel 计算

SanitizeByUserReview:
  条件: 用户 /approve 命令确认
  结果: data.Level = TaintUserReviewed, ApprovedBy = "user"

SanitizeByDeterministicTransform:
  条件: 数据经纯函数转换
  结果: data.Level = min(Level-1, TaintMedium)
  白名单: base64.DecodeString, hex.DecodeString, strconv.*, url.Parse + QueryUnescape, gzip.NewReader, crypto hash (SHA-256/BLAKE3)
  禁止: strings.Join/+=, text/template, regexp.ReplaceAll, exec.Command
  json.Unmarshal 不降级: 所有 string 字段完全继承输入字节流的原始 TaintLevel

**内容层注入检测（`ScanInjectionPatterns`）** — 与结构层 Taint 正交的第二道防线：
  实现见: `pkg/substrate/taint_sanitizer.go`
  触发位置: `SanitizeToSafe` 内部，结构层（Level 检查）通过后，TaintLevel >= TaintMedium 时执行。
  设计原则: 结构层（Taint Level）保证"来源可溯"，内容层（注入扫描）防止"受信源的恶意内容"绕过类型边界。

  `SanitizeToSafe` 执行两阶段防护：第一阶段（结构层）检查 TaintLevel，`TaintLevel > TaintLow` 且非 `TaintUserReviewed` 时直接拒绝；第二阶段（内容层）对 `TaintLevel >= TaintMedium` 的内容执行 `ScanInjectionPatterns`，命中任意高置信度注入模式则拒绝，未命中才构造 `SafeString{}`。

  `ScanInjectionPatterns` 规则集（16 条，OWASP LLM01 常见间接注入手法）：
  - 角色覆盖: `ignore previous instructions` / `ignore all previous` / `disregard previous` / `forget your instructions`
  - 人格劫持: `you are now` / `act as if you are` / `pretend you are`
  - 系统角色注入: `system:` / `<system>` / `[system]`
  - Markdown 指令注入: `### instruction` / `## new instruction` / `[inst]`
  - Token 边界注入: `</s>` / `<|im_start|>` / `<|im_end|>`

  扫描在 Unicode 归一化的小写文本上执行（折叠空白 + toLower），防止大小写/空格变体绕过。
  假阳性设计原则: 扫描只在 `TaintLevel >= TaintMedium` 启用（系统内部生成内容 TaintNone/TaintLow 不扫描），且均为高置信度模式（无正则），最大限度减少误报。
  `TaintUserReviewed` 来源绕过内容层扫描（人类已审查），直接构造 SafeString。

  **与 Spotlighting 的分工**（§2.2）：
  - Spotlighting: 运行时防护，将不可信数据用 `=== UNTRUSTED_DATA ===` 围栏包裹后注入 Prompt，防止 LLM 将其解析为指令。
  - ScanInjectionPatterns: 边界防护，在数据转变为 SafeString 的最后一步阻断已知注入特征，确保含高置信度注入指令的内容不能以 SafeString 形态进入任何 Instruction Slot。
  两者形成"注入到达 Prompt 前"（内容扫描）与"注入到达 Prompt 后"（Spotlighting）双重防线。

### 2.6 Agent Identity

身份: Ed25519 KeyPair，私钥种子持久化 OS Keychain；DID 格式 `did:web:agent.local:{pubkey_hash[:8]}`，首次启动生成，后续启动恢复。

能力声明: AgentCard（A2A v0.3）含公钥指纹/工具技能列表/最大并发/沙箱等级 + EdDSA 签名。远程 Agent 经 `/.well-known/agent-card.json` 发现，Ed25519 签名链验证通过才信任，失败则拒绝并审计。

---

## 3. Cedar 策略引擎 [Cedar-Gate]

Cedar: Rust 核心, CGO-Free FFI (purego) (<70ns overhead), <1ms 评估延迟, deny-by-default + forbid-overrides-permit + 形式化验证 (Lean)。CI 包含 Cedar FFI fuzz 测试。

> **已知缺口（Cedar FFI 静默降级可观测性）**：dylib 未加载时系统静默降级为 fail-open，当前无 `slog.Warn` 输出，监控无法从指标区分"Cedar 正常运行"与"Cedar 未加载降级"两条路径。修复方向：在 FFI 降级路径补充 `slog.Warn` + `polaris_cedar_degraded_total` Counter。

> **✅ 已修复（gate.go goroutine 泄漏）**：`IsAuthorized` 超时后新增 `cedarLeaks` 原子计数器追踪泄漏 goroutine；累计 ≥5 次时联动 KillSwitch Stage 1，使运营可感知并兜底；泄漏 goroutine 最终在 Cedar FFI 返回后自行退出（buffered channel 不阻塞）。Rust 侧 `cedar_evaluate` 应配套引入 timeout_ms 参数以从根本消除阻塞。

**Cedar FFI Failure Mode 表**:
| 失败场景 | 行为 | 审计 |
|---------|------|------|
| Init 失败 | fail-closed + 拒绝启动 (fatal) | CRITICAL |
| Evaluate panic | catch_unwind → deny + WARN | audit + `polaris_cedar_panic_total` Counter |
| Evaluate 超时 (>10ms) | deny + 增加 `polaris_cedar_timeout_total` Counter | WARN |
| 连续 10 次 Evaluate 失败 | KillSwitch Stage 1 THROTTLE | CRITICAL |

### 3.1 Cedar 策略结构

Agent 能力策略 (permit + conditions):
  read_only: trust_level >= 1 → permit
  write_local: trust_level >= 2 AND allowed_paths 含 target_path → permit
  write_network: trust_level >= 3 AND approval == "approved" AND cap_token_valid → permit
    附加 TaintLevel 约束: write_network 工具调用参数中任一 `[TaintLevel]` ≥ `[Taint-Medium]` → forbid (需经 SanitizeByUserReview 降至 TaintLow 或 TaintUserReviewed 后方可放行)
    附加配额约束 (OWASP LLM06 Excessive Agency 防护): Capability Token 必须包含 `MaxCallsPerTask` 维度（如单工具上限 50 次），杜绝无限制死循环代理。

**trust_level 数据来源**（插件/MCP 场景）:
  `skill_sources.trust_tier` / `mcp_servers.trust_tier` 在 Cedar 评估上下文注入为 `trust_level`；
  值由 `builtinCatalog` 白名单决定，安装时固化，请求方不可覆盖（ADR-0016）。
  运行时的 `write_network` 等危险操作评估，由 `trust_level` 结合全局 `permission_mode` 共同决断：
  - `full_access` 模式：TrustTier ≥ 2 自动通过。
  - `auto_review` 模式：TrustTier ≥ 3 自动通过，TrustTier = 2 需 `approval=="approved"`（HITL 补签）。
  - `default` 模式：所有外部扩展的危险操作强制需 `approval=="approved"`（HITL 补签）。
  - TrustTier = 1 时，所有模式均 deny-by-default，强行阻断。
  详见 M13 §8.6 插件安装流程。（**注意**：所有第三方或用户生成的扩展安装必须统一途经 `Manager.InstallExtension`，以确保上述策略被强制下发并执行）。

硬约束 (forbid 无条件优先):
  deploy/drop_db/delete_data/send_mass_email AND approval != "approved" → forbid
  monthly_spend > monthly_budget → forbid (所有 principal/action/resource)
  source == "llm_generated" AND capability == "write_network" → forbid

### 3.2 形式化验证

**CedarVerifier 启动时验证 (fail-closed)**:
启动期间执行策略检查。任何验证失败（如条件覆盖、预算配置冲突或越权规则）都会拒绝进程启动。

PolicyChaosTest (CI 门控):
  参数: numIterations=1000, 随机 (principal, action, resource, context)
  验证: 两次 Evaluate 一致 (确定性) + forbid 优先于 permit
  失败 → PR 自动拒绝

VerifyOnPolicyUpdate 热更新增量验证:
  新 policy 与已有 forbid 条件重叠 → reject
  新 forbid 与已有 permit 条件重叠 → reject

策略变更审批流程:
  VerifyOnPolicyUpdate → VerifyAtStartup → 人工多签 → 热更新部署
  运行时验证失败 → 原子回滚到上一个策略快照

策略热加载后任务处理:
  policy_version 原子递增加一
  M7 ToolRegistry 每次调用前比较 task.policy_version vs global.policy_version
  不一致 → CedarEngine.IsAuthorized 重新评估
  FORBID → 拒绝, policy_hotreload_revoked 审计事件, 任务回退 HITL

---

## 4. Kill Switch & Human-in-the-Loop

### 4.1 三阶段 FSM [KillSwitch]

KillSwitch/KillState 三阶段 FSM 实现见 `pkg/substrate/killswitch.go`。

| 阶段 | 触发条件 | 动作 | 恢复 |
|------|---------|------|------|
| Stage 1 THROTTLE | [TokenBurnRate] > 2x baseline (按任务分片 P95), 连续错误 > 5 | 降级 Tier 1 模型, max_steps=3, 禁止写操作 | 自动 |
| Stage 2 PAUSE | Stage 1 持续 > 10min, 安全违规 | 停止所有新任务, 保留状态, 通知 | 人工审批 |
| Stage 3 FULLSTOP | Stage 2 未在 15min 内审批, 致命违规, 管理员手动 | 停止所有 goroutine + LLM 调用, 写入 .fullstop | 管理员手动 unseal |

- **触发操作**: 生成 `.fullstop` 封存文件（含时间戳、原因、触发者）、所有 `Executing` 任务流转为 `Suspended` 挂起状态以供取证、立即中止所有 LLM 推理并在不可变日志中产生 `kill_event`。
- **约束要求**: 系统必须在 200ms 内部署完中止信号，通知所有可用告警渠道。

executePause: 200ms timeout → toolRegistry.StopAllPending

### 4.2 .fullstop 防守护进程重启循环

`substrate.IsFullStopFilePresent(dataDir)` 在 `main.go` 数据目录初始化完成后、任何服务启动前被调用。检测到 `dataDir/.fullstop` 存在时立即以错误退出，阻止系统以封印态重启并继续执行。

要从 FullStop 恢复：
1. 人工审查 `.fullstop` 文件内记录的触发原因和时间戳
2. 确认安全后手动删除 `dataDir/.fullstop`
3. 重新启动进程

封印态持久文件的内容为 JSON：`{"timestamp": <unix>, "reason": "...", "actor": "..."}`

### 4.3 物理触发路径

| 路径 | 机制 | 响应 |
|------|------|------|
| Ctrl+C x3 (3s 窗口) | SIGINT 计数器, 窗口重置归零, >=3 → Full Stop | <1s |
| ~/.polarisagi/polaris/KILLSWITCH 文件 | fsnotify 监视, 存在 → Full Stop | <500ms |
| POST /_admin/kill | localhost-only (127.0.0.1/::1), 无认证 | <100ms |
| [TokenBurnRate] > 10x baseline 30s | 滑动窗口背压熔断 | ~30s |
| Global DoS Guard (LLM10) | 全局信号量饱和 / Session Bucket 耗尽 | 限流或 Stage 1 |

TripleCtrlCGuard: 3s 滑动窗口计数 SIGINT, 归零/>=3 → executeFullStop

KILLSWITCHFileWatch: fsnotify 监视 ~/.polarisagi/polaris/KILLSWITCH, 存在 → executeFullStop, 删除后恢复

AdminKillEndpoint: 仅 127.0.0.1/::1, POST → executeFullStop; 否则 403

BurnRateFuse: 订阅 M3 `polaris_token_burn_rate` Gauge (CANONICAL SOURCE) → 当 EMA_30s > baseline.P95 × 10.0 持续 30s [Window-Burst-30s] → executeFullStop。计算逻辑由 M3 单源持有，M11 不独立采样。M3 暴露专用 Counter `polaris_token_burn_stage3_triggered_total`，KillSwitch 从该 Counter 边沿驱动。

Global DoS Guard (OWASP LLM10 Model DoS 防护): 两层遏制——
  (1) 全局信号量: 全系统并发 LLM 调用上限（Tier 0=4）
  (2) Session Token Bucket: 单任务/会话请求频次约束
超限 → HTTP 429 / 局部排队；持续强刷 → 晋级 KillSwitch Stage 1 (THROTTLE)。

### 4.4 ESCALATE.md 协议 [ESCALATE]

escalate.yaml:
  always_escalate: [deploy_to_production, send_external_communication, financial_transaction, delete_data, privilege_change, cost_exceeds_usd: 100.00]
  channels:
    slack: "#ai-alerts", timeout: 10min
    email: "ai-ops@company.com", timeout: 30min
  approval:
    timeout: 见 `spec/state.yaml §m11_policy.escalation_timeout_minutes`
    on_timeout: escalate_to_killswitch (Stage 3)
    on_denial: halt_and_log
    on_approval: proceed_and_log

---

## 5. 数据隐私与凭证安全

### 5.1 PII 检测与红化 [PIIGuard]

PIIGuard:
  组件: PIIDetector (Go 原生正则 + 规则引擎 + 可选 Presidio sidecar) + SessionTokenizer (会话级 token 映射)
  Tier 0: Go 原生正则 (<1ms)，覆盖 P0 结构化模式（信用卡/SSN/API Key/邮箱/手机/IP）。
  Tier 1+: 显式启用 Presidio sidecar，高精度 NER。`FeatureGate.FeaturePresidioPII` 自动化（≥Tier1 且 ≥512MB free→启用），详见 M03 §5。

  **Tier 0 降级行为契约**: 仅保证结构化 PII 检测；非结构化 PII（姓名/地址/出生日期/雇主/医疗/生物特征/行为画像/家庭关系）不保护，会进 LLM prompt。
  首次进入 PII 场景（开启 Notion/Gmail Connector 等）主动告警："Tier 0 仅基础防护，建议升级 Tier 1+ 启用 Presidio"。

  RedactMode:
    RedactBlock: 含 PII → ErrPIIDetected, 阻止执行
    RedactReplace: 含 PII → tokenizer.Replace 替换为会话 token [TYPE_N]
    RedactWarn: 含 PII → warn 日志继续

  **PIIGuard 双向防护**: PIIGuard 同时在输入端（M4→M7 工具参数 SecureUnredact 之前）和输出端（M7 ToolResult→EventLog PostExecution Redact，M7 §4.3 Step 5）工作。输入端阻止 PII 进入 LLM Provider，输出端阻止 PII 进入不可变审计轨迹。Tier 0 仅保证结构化 PII 模式检测覆盖两端。

SessionPIIVault:
  RedactAndVault(ctx, fieldPath, value) → 存入 per-session 内存 Vault (key=sessionID, tokenID), 返回 OpaqueToken; 结构化字段替换为 {"$pii_ref": "tokenID"}
  SecureUnredact(ctx, tokenID, fieldPath, authProof) → 验证: (a) tokenID 属当前 session, (b) Capability Token 持有 pii_resolve, (c) fieldPath 在 InputSchema 声明 x-polaris-pii: true
  shell.exec/python.run 等自由命令字符串的 command 字段不声明 x-polaris-pii, SecureUnredact 拒绝解析
  单 session vault 硬上限 1MB（约 10000 个 token），超限 → 拒绝新 PII 检测 + WARN
  Session 关闭事件 → vault.Destroy 立即销毁 (不可恢复), 满足 GDPR 数据最小化
  M3 暴露 polaris_pii_vault_size_mb gauge 监控

  **跨会话挂起持久化（解决 Suspended 任务 PII 丢失）**:
  SuspendSnapshot(ctx, taskID):
    token map (tokenID→PII 明文) → msgpack → AES-256-GCM + persistent_key 加密（nonce 随机，prepend 密文头）
    ≤4KB → tasks.pii_vault_blob 列 (base64)；>4KB → VFS blob (`~/.polarisagi/polaris/vfs/`) + tasks.vfs_ref (VFS refcount+1)
    落盘: MutationIntent{Table:"tasks", Op:OpPatch, Payload:{pii_vault_blob:...}} 经 MutationBus 串行
    落盘成功 → 立即清零内存 vault（GDPR：明文不持久驻留）
    架构约束: 仅 Tier-0 单机有效；persistent_key 源 OS Keychain，跨机迁移不可解密——已知限制
  RestoreFromSnapshot(ctx, taskID):
    读 tasks.pii_vault_blob（vfs_ref 则从 VFS 读完整 blob，VFS refcount-1 入队 GC）
    AES-256-GCM 解密 → 反序列化 token map → 重建内存 Vault（绑定新 sessionID）
    成功 → 原子清除 tasks.pii_vault_blob (MutationIntent OpPatch null)，Vault 回归内存态
    失败 (blob 损坏 / key 轮换) → ErrPIIVaultRestoreFailed + CRITICAL，任务强制 S_FAILED
  SecureZero(ctx, taskID):
    任务转 Done/Failed 时调用。原子: (1) 清零 tasks.pii_vault_blob (2) VFS blob 解引用→refcount=0 自动 GC (3) 写 pii_vault_purged 审计
    M4 FSM 转 S_FAILED/S_COMPLETE 时调用，早于 M2 WorkspaceManager GC

### 5.2 Credential Vault [CredentialVault]

SecretBackend OS 密钥链抽象:
  Get(key) → ([]byte, error), 未找到 → ErrSecretNotFound, 调用方清零
  Set(key, value) → error, 写入后调用方清零 value
  Delete(key) → error, 幂等
  List() → ([]string, error), 不返回凭证值
  延迟: Get/Set <5ms, List <20ms

平台适配:
  macOS: Security Framework (keychain)
  Linux: Secret Service API (libsecret/gnome-keyring)
  Windows: Credential Manager API (credential.h)
  Fallback: age-encrypted file (password-derived key)
  依赖: github.com/99designs/keyring, 优先级: macOS Keychain > Windows Credential Manager > Linux kernel keyring > encrypted file

安全原则:
  凭证永不落盘明文, 仅 OS 密钥链获取, 运行时驻留内存
  使用后立即清零 (subtle.ConstantTimeCopy + manual memclr)
  审计日志记录访问事件 (谁/何时/什么凭证) 不记录凭证内容

**persistent_key 轮换**:
  触发: `polaris vault rotate-master-key`
  流程: 后台分批解密旧 HMAC/Workspace/pii_vault_blob → 新 key 重加密（pii_vault_blob 扫描 tasks 表非 null，逐条 MutationBus 原子更新）→ 双 key 共存窗口（新写新 key，读 new→old fallback）→ 旧 key 销毁

**冷启动决策树**:
  - GUI 桌面 → OS Keychain（首次弹窗授权）
  - headless Linux → age-encrypted file，password 从 stdin 或 `POLARIS_VAULT_PASSPHRASE`（启动期立即清零）
  - Docker → 挂载 secrets volume / docker secret
  - 首次 password: `polaris vault init` 交互式引导

### 5.3 local_only 网络沙箱三层防御

双层隔离:
  - OS 级沙箱（macOS sandbox-exec / Linux Landlock LSM / Windows WFP）— 网络隔离唯一有效防线（子进程独立 OS 进程，SafeDialer 仅 Go 进程内有效）
  - Go 层纵深: RoundTripper 替换 no-op transport + DefaultResolver 覆写 NXDOMAIN + Dialer.Control 拒非 loopback IP
启动期自检: DNS 解析公网域名 → 有响应 → 拒绝启动 (fail-closed)。

Tier 3 本地模型守卫: M1 LocalProvider.Probe() 验证可加载模型且峰值 RSS + 已用内存 < 64GB (1GB 预留)，否则拒绝 local_only。

白名单 `local_only_network_allowlist.toml` (用户 Ed25519 签名): 仅 M10 Connector 豁免，Tier 3 上限 5 条，变更需重启。Rust FFI 引擎 (SurrealDB-Core/Cedar) 无网络能力；Tier 0/1/2 彻底禁用 local_only，不进行加载。

---

## 6. SSRF 与 DNS Rebinding 防护 [SSRFGuard]

blockedCIDRs: 127.0.0.0/8 / 10.0.0.0/8 / 100.64.0.0/10（CGNAT，已补充）/ 172.16.0.0/12 / 192.168.0.0/16 / 169.254.0.0/16 / ::1/128 / fc00::/7 / fe80::/10。dnsCache TTL 见 `spec/state.yaml §m11_policy.safe_dialer_dns_cache_ttl_seconds`。

> **已修复（CGNAT 段补充）**：`safe_dialer.go` 已在 blockedCIDRs 补充 `100.64.0.0/10`（CGNAT 共享地址段，此前遗漏可被用于 SSRF 绕过）。

五阶段: Phase 0 — Capability 出口强制 (read_only 禁止写 HTTP / write_local 仅内网) → Phase 1: DNS 解析 → Phase 2: blockedCIDRs 校验 → Phase 3: TOCTOU 延迟（`spec/state.yaml §m11_policy.safe_dialer_toctou_delay_ms`）后二次 DNS 解析 + blockedCIDRs 校验 → Phase 3.5: 响应 IP 超阈值（`spec/state.yaml §m11_policy.safe_dialer_max_ips_threshold`）→ 拒绝 → Phase 4: DNS TOCTOU 消除（验证通过后覆写 DialContext 锁定 IP，Request.Host 保留原始 hostname）。

与 M7 协作: M7 做 URL/IP 静态校验 + Capability 声明层收缩，M11 做出口强制执行 + DNS Rebinding 动态检测 + IP 锁定。两层纵深防御: 声明层(M7) → 网络出口层(M11 Phase 0)。

**统一安全 Dialer** (`internal/protocol/interfaces.go` SafeDialer):
  M11 导出 SafeDialer.DialContext。四层注入覆盖全出站: http.Transport.DialContext / grpc.WithContextDialer / websocket.Dialer.NetDialContext / net.DefaultDialer.Control
  DialContext 内执行五阶段 SSRF (Phase 0-4)。
  Taint 出口拦截: 调用方在 DialContext 前显式调用 `SafeDialer.TaintEgressCheck(taintLevels)`，`[Taint-Medium]` 及以上级别（TaintMedium/TaintHigh）未经 SanitizeByUserReview → ErrTaintBlockedEgress。Gate.TaintEgressCheck 与 SafeDialer.TaintEgressCheck 采用同一阈值（`>= TaintMedium`），两层一致防止出口绕过。
  两层纵深: M7 Policy Gate4（声明层预检）+ M11 SafeDialer.TaintEgressCheck（出口层终检，调用方职责）。
  M7/M10/M13 所有出站必经此入口。CI `safe_dialer_lint` 扫描裸 `net.Dial` / `grpc.Dial` / `http.Get` → ERROR。
  **✅ 已修复（SurrealStore 出口缺失）**：Go 侧 SurrealStore wrapper 在调用 `surreal_store_insert` / `surreal_store_query` FFI 前补充 `SafeDialer.TaintEgressCheck`，确保 TaintHigh 数据不绕过出口拦截直写认知存储。

---

## 6.5 D6 防线：`[FactualityGuard]` 输出真实性核验

> **inv_global_06**: 与 PII Guard 并列守护 LLM 输出边界。D1~D5 守护输入与权限，D6 守护**输出事实性**。

**实现**: `pkg/substrate/policy/factuality_guard.go`

### 三层核验机制（已实现）

LLM 输出触发抽样核验（TaintHigh 强制，其余 10% 抽样）：

- **L1 CitationCheck**（确定性）：检测 content 中的具体引用标记（"according to", "source:" 等），关键词必须在 contextDoc 中出现；长数字串（≥5位）若在 context 中无出处则标记 Uncertain。
- **L2 NumericalConsistency**（确定性）：检测概率/精度值超 100%（"accuracy 110%" → Fail）；年份合理性（<1900 或 >2100 → Uncertain）；更多约束可扩展。
- **L3 SemanticJudge**（抽样 + LLM）：仅 TaintHigh 内容且 llmProvider 非 nil 时触发。调用独立 Provider 一次推理，返回 PASS/UNCERTAIN/FAIL。超时或故障 → Uncertain（不阻断）。Tier 0 无 Provider 注入时 L3 pass-through。

### 抽样策略

- TaintHigh 内容：强制三层全检（抽样率 1.0）
- 其余内容：默认抽样率 0.1，可在构造时覆盖
- 结果路由：FactualityFail → 降级消息 + OnFail 回调；Uncertain → 低置信度标记不阻断；Pass → 继续

### Taint 跨边界 HMAC 验证（inv_M11_02）

`TaintBoundarySerializer`（`pkg/substrate/taint.go`）：跨模块传输污点数据时，Seal 附加 HMAC-SHA256（覆盖内容 + 污点等级 + 来源实体 ID），Unseal 时重新计算并比对；HMAC 不匹配则强制将污点升级到 TaintHigh，防止反序列化路径绕过污点标记（降级攻击）。

---

## 7. 不可变审计轨迹

**实现**: `pkg/substrate/audit_trail.go`

### 7.1 Append-Only Hash Chain

每条 `AuditRecord` 包含：事件 ID / Unix μs 时间戳 / Agent ID / Session ID / 操作类型 + 详情 / 信任等级 / 授权来源 / 操作结果（allow/deny/error/escalated）/ 拒绝原因 / PII 标志 / PrevHash / RecordHash。

Hash Chain 结构：`RecordHash = SHA-256(序列化后记录，不含 RecordHash 字段本身)`，`PrevHash(i) = RecordHash(i-1)`，首条 PrevHash 为空字符串。所有记录持久化到 `events` 表（topic='audit.policy'），DDL 层 trigger 禁止 UPDATE/DELETE（append-only 强制）。

`VerifyIntegrity()` 遍历内存链逐条重算 RecordHash 并比对 PrevHash 链接，返回 (ok bool, brokenIndex int)。同时在 `RecoverOnStartup()` 中对从 DB 恢复的尾部 100 条记录执行完整性校验，不通过则拒绝启动。

**DB 层 Hash Chain（全事件覆盖）**：`events` 表新增 `hash`/`prev_hash` 列，由 `DatabaseWriter.executeInsertEvent` 在 INSERT 时同步计算：`hash = SHA-256(id||topic||actor||type||payload||prev_hash)`。提供独立于内存审计链的持久可验证层，覆盖全表事件（不限 audit.policy topic）。两条链的关系：内存审计链守护 audit.policy 事件实时可验证；DB 层 hash chain 是完整性备份，崩溃恢复后可验证全量历史事件。

### 7.2 Epoch 轮转

触发：审计日志估算体积 > 100MB（由调用方传入当前 MB 数）。封存流程：追加 `epoch_end` 标记记录（FinalHash + RecordCount），写 DB；更新 epochID；追加 `epoch_start` 标记（PrevEpochFinalHash），建立跨 Epoch 密码学连续性。归档目录 `~/.polarisagi/polaris/audit/archive/`，保留 90 天（Tier 0）。

### 7.3 Outbox Worker 增量消费（HE-Rule-6）

`pkg/substrate/outbox_worker.go` 实现 `OutboxWorker.Run(ctx)` 主循环：游标（`outbox_cursor`）持久化到 `sys_config` 表，重启后从 DB 恢复防止漏消费；每批处理后原子 CAS 更新游标（Exactly-Once 语义）；失败记录指数退避（2^attempt × 5s），连续崩溃 ≥3 次标记 dead（毒丸清除）；ReplayMode 时跳过所有副作用。

---

## 8. 多 Agent 宪法分层

Layer 4: Agent 间交互规则 (在 §1 三层宪法之上):
  - Agent 间信息传递边界
  - 任务委托链深度上限
  - 跨 Agent 权限组合约束
  - 黑板消息最小权限路由

Cedar 策略扩展 (Layer 4):
  forbid send_message → BlackboardEvent: payload 含 cross_agent_prohibited_data AND principal.id != source_agent_id
  forbid delegate_task: delegation_chain_depth >= 3
  forbid call_tool: capability == "write_network" AND 任一 collaborating_agent trust_level < 3

**JIT Token 委托能力收缩**：`ValidateDelegation`（`pkg/action/capability_token.go`）强制执行权限收缩规则——effectiveCaps = intersect(parentToken.Claims.Caps, requestedCaps)；交集为空时返回 `ErrCapabilityInsufficient`；子 Agent 永远无法获得超过父 Token 所持有的能力。委托链深度上限 3 层由 depth 参数控制，父 Token 有效性先经 `TokenManager.Verify()` 确认。

黑板消息最小权限路由:

| Trust Level | 可接收消息类别 |
|-------------|--------------|
| 1 | 只读任务描述, 结构化输出 |
| 2 | + 文件内容 (tainted 标记保留) |
| 3 | + 用户原始输入, 跨 Agent 上下文 |
| 4 | + 安全事件, 审计日志 |
| 5 | 全部 |

---

## 9. 运行时威胁监控

SafetyMonitor 整合 TaintGate / CedarEngine / KillSwitch 事件流，提供统一安全态势感知。

事件分类: 污点违规 / 策略拒绝 / Token 燃烧速率飙升 / 权限提升尝试 / 沙箱逃逸尝试。严重级别: info / warning / critical。
各组件 → 集中 safetyEvents channel；Monitor 30s 滑动窗口关联分析——同类事件 >3 次 → warning 自动升级 critical。

响应:
  - critical → Audit Trail + 全渠道通知；sandbox_escape_attempt → KillSwitch Stage 2
  - warning  → Audit Trail + 日志告警
  - info     → 仅日志

---

## 13. 降级与失败模式（5 问全覆盖）

| 故障 | (Q1) 检测 | (Q2) 影响范围 | (Q3) 即时反应 | (Q4) 自动恢复 | (Q5) 人工介入触发 |
|------|----------|------------|------------|------------|----------------|
| Cedar FFI crash/不可用 | FFI panic / 连续 Evaluate 失败 | 全策略路径 | **fail-closed**（拒绝所有 action） | 否 | 必须 HITL |
| 签名验证失败 | sig mismatch | 单 token | 拒绝执行，写 audit severity=critical | 否 | 累计 ≥3 次 → escalate |
| audit_log 写入失败 | DB error | 全模块副作用 | **fail-closed**（暂停所有副作用直至 audit 恢复） | 否 | 必须 HITL |
| Capability 注册表损坏 | DB integrity check | 全策略路径 | 退到 Hard 默认拒绝 | 否 | HITL |
| Hash chain 断裂 | VerifyChain 失败 | audit 完整性验证 | 标记 tampered + 立即 fail-closed | 否 | 必须 HITL（取证） |
| Taint high_ratio 超阈值 | M3 metric > 60% | 数据治理 | M3 告警，**不自动降级** | 否 | HITL review |
| Kill Switch 误触发 | trigger 来源 audit | 全系统 | 各模块按协议响应；等用户解除 | 部分 | 必须 HITL |
| Cedar 策略热更失败 | 编译错误 | 单规则 | 拒绝新版本，保留旧版本继续运行 | 是 | 告警提示 |
| SafeDialer DNS 解析超时 | context deadline | 单连接 | 拒绝该连接 + ErrDNSUnreachable | DNS 恢复后自动重试 | — |
| TaintTracker 传播计算阻塞 | goroutine 调度延迟 | 污点传播路径 | L1: 跳过非关键数据 / L2: 全部标记 TaintHigh | 是 | 持续阻塞 > 30s → audit |

M11 故障默认 fail-closed (拒绝执行)，保障安全。与 OSMemoryGuard 协同: L3 临界 → KillSwitch 自动触达 Stage 3 FullStop。


## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m11_policy`。

## 管理端点认证兜底（无全局 API Key 场景）

`POLARIS_API_KEY` 未设置时，`/v1/` 接口对所有来源开放。**CORS 设为 `*` 的情况下，局域网内任意网页可调用管理写接口**，构成安全风险。

强制要求：

- 无 API Key 时，启动日志必须打印 `WARN: admin write paths restricted to localhost only`
- 写操作管理端点（`POST/PUT/DELETE /v1/mcp-servers`、`POST /v1/plugins/install` 等）在无认证时**仅允许 localhost (127.x / ::1) 来源**，非 localhost → 403
- 上述逻辑在 `withMiddleware` 中通过 `isAdminWrite` + `isLoopback` 判断实现，不得删除

## 14. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M2 Storage Fabric | EventLog 审计轨迹写入、MutationBus 串行写、Outbox 模式 | M2 §2.1, §2.3, §2.5 |
| M3 Observability | TokenBurnRate CANONICAL SOURCE → M11 KillSwitch 熔断 | M3 §3 |
| M4 Agent Kernel | TaintGate Layer A/A.1/B、CheckBurnStatus 仅响应不触发 | M4 §3, §7 |
| M7 Tool & Action | Capability Token JIT Minting、SafetyMonitor 事件来源 | M7 §6, §4 |
| M8 Orchestrator | KillSwitch FullStop → orchestrator.StopAll | M8 §1.7 |
| M10 Knowledge RAG | Connector Taint 初始等级查找表 | M10 §0, M11 §2.4 |
| 全局字典 | TaintLevel/Taint-Prop/Taint-Sanitizer/KillSwitch 完整定义 | 00-Global-Dictionary §4, §5, §8 |
| DDL | 001_events（审计轨迹 source）、006_decision_log（决策日志） | internal/protocol/schema/ |
| 时序图 | KillSwitch 触发与响应链全流程 | DIAGRAMS.md#killswitch |
