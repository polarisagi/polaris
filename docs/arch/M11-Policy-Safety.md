# 模块 11: Policy & Safety

> Go + Rust(Cedar CGO-Free FFI (purego)) | [Module-Topology] L0 | [Code-Package-Mapping] internal/
> 设计约束: 三层宪法 + Taint Tracking 主防线 + Cedar 策略引擎 + KillSwitch | [HE-Rule-2] 可验证执行
> 更新日期: 2026-04-30
<!-- §跳读: 0:10 职责 / 0-ter:47 不变量速查 / 1:60 三层宪法 / 2:88 Taint / 3:227 Cedar / 4:293 KillSwitch / 5:371 隐私 / 6:441 SSRF（Server-Side Request Forgery，服务端请求伪造） / 6.5:446 Factuality / 7:493 审计 / 8:517 多Agent宪法 / 9:544 威胁监控 / 13:558 降级 / 14:590 跨模块契约 -->

---

## 0. 职责边界

| M11 **是** | M11 **不是** |
|-----------|-------------|
| 策略评估引擎（allow/deny/redact） | 业务逻辑实现者 |
| Capability Token 签发与撤销中心（短寿命 Ed25519） | 长期凭据持有者 |
| 沙箱分级规则定义（Sbx-L1/L2/L3 判定依据: TrustTier + RiskLevel） | 沙箱执行器（那是 M7 wazero/gVisor），沙箱选型由 M7 ToolRegistry 指定 |
| 污点标签传播规则（Taint 5 级 + PropagateTaint） | 污点数据存储者（那是 M2 events/chunks 表 TaintLevel 列） |
| 安全事件审计源（内存 HashChain 防篡改 + DB（Database，数据库） 永久化存档） | 通用业务日志（那是 M3） |
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
| **D1** | 数据污点追踪 | Taint 5 级 + Slot 物理分离 | 输入 | `internal/security/` |
| **D2** | 能力令牌 | 短寿命 Ed25519 + 最小权限 + 委托链 ≤3 | 权限 | `internal/action/capability_token.go` |
| **D3** | 沙箱分级 | Sbx-L1(InProc) / L2(Rust 脚本沙箱) / L3(平台原生 microVM: Linux Firecracker / macOS VZ / Windows WSL2，gVisor 仅作 Linux KVM 不可用 fallback) | 执行 | `internal/sandbox/sandbox_impl.go` |
| **D4** | 宪法分层 | Layer 1(编译期常量) / 2(Cedar forbid) / 3(Cedar permit) / 4(多 Agent) | 决策 | `internal/security/` |
| **D5** | Kill Switch + Audit | 三阶段 FSM（Finite State Machine，有限状态机） + hash chain 仅追加 | 系统 | `internal/security/` |
| **D6** | [FactualityGuard] | 引用核验 + 数值一致性 + 抽样 LLM（Large Language Model，大语言模型）-as-Judge | **输出真实性** | `internal/security/` |

核心原则：**拒绝绝对化**——不承诺零突破，承诺"突破必留痕"（audit hash chain 不可篡改）。

**D1~D5 守护输入与权限边界；D6 (inv_global_06) 与 PII（Personally Identifiable Information，个人敏感信息） Guard 并列守护输出边界——LLM 输出的事实性。** 详见 §6.5。

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M11_01 | 五条防线独立生效——任一条失效其余仍能阻挡（纵深防御） | 安全审计逐条验证 |
| inv_M11_02 | TaintLevel 只升不降——output = max(inputs)，受控降级仅四种 Sanitizer | CI（Continuous Integration，持续集成） `taint_propagation` 测试 |
| inv_M11_03 | Audit Trail 仅追加不可篡改——hash chain + DDL（Data Definition Language，数据定义语言） 触发器禁止 UPDATE/DELETE | M11 §7.1 VerifyIntegrity |
| inv_M11_04 | KillSwitch FULLSTOP 200ms 内停止所有 tool call——`.fullstop` 持久文件防守护进程重启循环 | M11 §4.1 executeFullStop |
| inv_M11_05 | 所有出站连接经 SafeDialer.DialContext 五阶段 SSRF 防护——HTTP（HyperText Transfer Protocol，超文本传输协议）/3 QUIC 禁用 | CI `safe_dialer_lint` |
| inv_M11_06 | 拒绝绝对化——不承诺零突破，承诺"突破必留痕"（audit hash chain） | M11 ADR-010 |

---

## 1. 三层宪法架构

**Layer 1 不可侵犯条款 — 编译期常量**:
- `AuditLogAlwaysOn = true`
- `KillSwitchEndpoint = "/_admin/kill"`
- `SelfModificationGuard = true`
- **TestInviolableConstants 编译期验证**: 上述常量若移除或置 false → 编译/测试失败

**L4 运行时启动期保护**: 
M11 进程启动后第一时间计算核心安全相关源码目录及文件（包括 M4 Agent Kernel 的 `internal/agent/`、`internal/security/`、`internal/sandbox/`、`internal/config/immutable_constants.go`、`internal/protocol/schema/` 等）的当前 SHA-256 哈希集合，并与构建时通过 ldflags 注入（或 go:embed 静态绑定）在二进制文件内的权威 manifest 进行比对。若哈希值不一致（表明运行时源码已被篡改），则触发 fail-closed 拒绝启动并生成 CRITICAL 级别的审计日志。`internal/protocol/interfaces.go` 定义安全关键接口（SafeDialer/Cedar-Gate/TaintLevel/Blackboard），`schema/` 定义 EventLog DDL——两者是安全链的契约基础，篡改即整个安全模型失效。

**额外防御**: 禁止 `internal/swarm/` 路径下使用 `unsafe` / `reflect.Value.UnsafePointer` / CGO（CI lint 强制）；进程启动期 `SetGCPercent` + `SetMemoryLimit` 锁定 runtime 参数，禁止运行时通过 reflect 修改。关键二进制可选 TPM/Secure Enclave 锚定（Tier 2+ 选配）。

**Layer 2 硬约束 — Cedar forbid (无条件优先 permit)**:
- **forbid**: 不可逆操作未经审批 → `resource.tool_name in [deploy_to_production, delete_data, send_external_communication, financial_transaction] AND context.approval_status != "approved"`
- **forbid**: LLM 生成代码执行特权操作 → `principal in Role::"Agent" AND resource.source == "llm_generated" AND resource.risk_level == "privileged"`
- **forbid**: 预算硬上限 → `context.monthly_spend_usd > context.monthly_budget_usd` (所有 principal/action/resource 无条件)
- **forbid**: Holdout Set 读取隔离 → `principal in Role::"Agent" AND action in [Action::"read_local", Action::"read_file"] AND resource.path.startsWith(context.polarisagi/polaris_eval_holdout_path)`
  - *说明*: `context.polarisagi/polaris_eval_holdout_path` 由 Go 侧在策略加载时注入（展开后的绝对路径，等价于 `~/.polarisagi/polaris/eval/holdout/`）。`ci_gate` role 不受此 forbid 限制（CI/Canary 需要读取 Holdout Set）。此规则为防御纵深——物理隔离层（WASI 沙箱 + Openat2 `RESOLVE_IN_ROOT`）已阻止逃逸，Cedar 规则覆盖 Host Function 层可能的访问向量。

**Layer 3 软约束 — Cedar permit + conditions**:
- **permit**: 只读工具 → `trust_level >= 1`
- **permit**: 本地写入 → `trust_level >= 2 AND resource.allowed_paths 包含 context.target_path`
- **permit**: 网络操作 → `trust_level >= 3 AND context.approval_status == "approved" AND context.capability_token_valid`
> 可热更新, 无需重启

---

## 2. Prompt Injection Defense — Taint Tracking 主防线

### 2.1 TaintedString / SafeString 类型系统 (四重防护)

TaintLevel/TaintedString/SafeString/TaintSource/PropagateTaint 类型定义见 `internal/security/`。

四重防护说明：

| 防护层 | 机制 | 执行位置 |
|-------|------|---------|
| 第一重：编译期类型断裂 | `TaintedString` 与 `SafeString` 内容字段均不导出；`PromptBuilder.WriteInstruction` 仅接受 `SafeString`，`TaintedString` 无法直接进入 Instruction Slot | 编译期 Go 类型系统强制 |
| 第二重：CI 静态分析 | `go-vet taint_enforcement` 扫描：taint 包外构造裸 `SafeString`、包外访问 `TaintedString.content`、`string` 直传 `WriteInstruction` 均报 ERROR | CI 门控，PR 拒绝 |
| 第三重：持久化边界密码学验证 | SQL/JSON/Protobuf 序列化层附加 HMAC-SHA256（`persistent_key` OS（Operating System，操作系统） Keychain 派生），反序列化时重算验证；验证失败或字段缺失 → 强制 `TaintHigh` + CRITICAL 审计；启动顺序 `CredentialVault.Init() → StorageFabric.Open()`，超时 30s 进程退出 | 运行时序列化钩子 |
| 第四重：泛型反序列化防剥离 | 禁止 `json.Unmarshal` 到 `map[string]interface{}` / `any` 等弱类型集合；外部 JSON 须反序列化到显式声明的带污点结构体 | CI go-vet lint 强制 |

### 2.2 辅助防线 (OWASP LLM Top 10 2025 防护)

执行顺序: Taint Gate → AnomalyDistanceFilter → Spotlighting → SIC（System Instruction Check，系统指令检查） Cleaning → SystemPromptGuard → Capability Gate

**Spotlighting**:
- 步骤1: 生成标记 = SHA-256(content)[:8]（内容派生，非随机——保证重放确定性，M12 Eval 回放验证一致性）
- 步骤2: `"=== UNTRUSTED_DATA_{hex} ===\n{data}\n=== END_UNTRUSTED_DATA ==="`
- 调用方: `kernel.PromptBuilder.WriteUserData`（ZoneTaintedData 写入前强制包裹）

**SIC CleanInstructions** (maxIter=5):
- 步骤1 detect: LLM 检测 override/extract/reset/伪系统指令模式 → bool
- 步骤2 rewrite: 替换为安全标记
- 步骤3 iterate: 连续两次文本相同 → 完成
- 步骤4 bailout: 达 maxIter 仍检测到 → `ErrUncleanableContent`

**SystemPromptGuard** (防系统提示词泄露 - OWASP LLM07):
- 实现: `internal/security/guard/system_prompt_guard.go`
- 触发: 出站文本（用户终端回复 + write_network 流量）调用 Scan 前注册系统提示池（AddFragment）。
- 机制: 滑动窗口匹配，发现连续重合 ≥ 15 tokens 的片段触发阻断（可升级为 Aho-Corasick）。
- 结果: `redact=true` 将片段替换为 `[SYSTEM_REDACTED]`；`redact=false` 返回 `ErrPromptLeakage` 拒绝出站。

**AnomalyDistanceFilter** (对角马氏距离异常检测 - OWASP LLM08):
- 实现: `internal/security/guard/anomaly_distance_filter.go`
- 触发: 外部非结构化输入进入 Infer 前调用 Check。
- 机制: Welford 在线统计（均值/方差），对角近似马氏距离（z-score 平方和），阈值 3σ；样本数 < 30 自动 bypass（冷启动保护）。
- 结果: 异常 → `TaintHigh` + `ErrAnomalyDetected`；样本不足 → `TaintMedium` bypass。

### 2.3 TaintLevel 五级量化 [TaintLevel]

| 级别 | 语义 | 来源 | Slot |
|------|------|------|------|
| TaintNone (0) | 系统编译期内容 | system slot / compiled prompts | instruction |
| TaintLow (1) | 用户原始输入 | instruction slot / workspace 内文件 | instruction |
| TaintMedium (2) | 受信第三方输出 | 白名单 MCP（Model Context Protocol，模型上下文协议） / allowlist connector | data (不可进 instruction) |
| TaintHigh (3) | 不受信外部内容 | 抓取网页 / 未知 MCP / shell stdout | data (不可进 instruction) |
| TaintUserReviewed (4) | 用户显式 review 后降级 | 用户确认操作 | data (不可进 instruction) |

### 2.4 传播规则 [Taint-Prop]

PropagateTaint: `output = max(所有输入的 TaintLevel)`, 只升不降

**受控降级 Downgrade** (仅以下路径):
- **用户显式确认**: high/medium → user_reviewed
- **Schema 校验**: high → medium (预定义 JSON Schema)
- **确定性转换**: high → medium (白名单纯函数)
- **LLM 摘要**: high → medium / medium → medium (TaintMedium 硬地板 `[Taint-Floor-Medium]`)

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

**SanitizeBySchema**:
- 条件: 数据通过预定义 JSON Schema 校验
- 结果: `data.Level = min(Level-1, TaintMedium)`
- 字符串字段附加约束: 仅当字段定义了 format / pattern / enum / const 时允许降级；裸 `{"type":"string"}` 不降级。数值/布尔/受限枚举字段不受此限制。
- 嵌套结构: 任一深层子节点为无约束裸 string → 整个父结构不可整体降级（`[Taint-Prop] max(inputs)`）。
- 审计: 每次降级写 audit_log，标注依据（format_guard / pattern_guard / enum_guard / const_guard / type_only）。

**SanitizeBySummarization**:
- 条件: LLM 对 tainted 数据做摘要
- 结果: `data.Level = max(min(Level-1, TaintMedium), TaintMedium)`
- 硬地板: TaintMedium, 摘要永不进入 instruction slot
- M4 Layer A 豁免: 系统自生成摘要 (`source='compaction'/'persona_refinement'/'consolidation'/'skill_compilation'`) 标记 TaintMedium 但不参与 `ActiveContext.TaintLevel` 计算

**SanitizeByUserReview**:
- 条件: 用户 `/approve` 命令确认
- 结果: `data.Level = TaintUserReviewed`, `ApprovedBy = "user"`

**SanitizeByDeterministicTransform**:
- 条件: 数据经纯函数转换
- 结果: `data.Level = min(Level-1, TaintMedium)`
- 白名单: `base64.DecodeString`, `hex.DecodeString`, `strconv.*`, `url.Parse + QueryUnescape`, `gzip.NewReader`, crypto hash (`SHA-256/BLAKE3`)
- 禁止: `strings.Join/+=`, `text/template`, `regexp.ReplaceAll`, `exec.Command`
- `json.Unmarshal` 不降级: 所有 string 字段完全继承输入字节流的原始 TaintLevel

**内容层注入检测（`ScanInjectionPatterns`）** — 与结构层 Taint 正交的第二道防线：
  实现见: `internal/security/`
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

能力声明: AgentCard（A2A（Agent-to-Agent，智能体间通信） v0.3）含公钥指纹/工具技能列表/最大并发/沙箱等级 + EdDSA 签名。远程 Agent 经 `/.well-known/agent-card.json` 发现，Ed25519 签名链验证通过才信任，失败则拒绝并审计。

---

## 3. Cedar 策略引擎 [Cedar-Gate]

Cedar: Rust 核心, CGO-Free FFI (purego) (<70ns overhead), <1ms 评估延迟, deny-by-default + forbid-overrides-permit + 形式化验证 (Lean)。CI 包含 Cedar FFI fuzz 测试。

> **已修复（Cedar FFI 降级可观测性）**：`gate.go` 第 434 行 FFI 失败时已输出 `slog.WarnContext(ctx, "cedar ffi failed, degrading to go rules", "error", err)`，降级路径可观测。`polaris_cedar_degraded_total` Counter 已实现：`internal/observability/metrics/metrics.go` 定义该 Counter，`internal/security/policy/gate.go` 超时路径与 FFI 报错路径均已埋点 `metrics.GlobalCedarDegradedTotal.Add(1)`。

> **✅ 已修复（gate.go goroutine 泄漏）**：`IsAuthorized` 超时后新增 `cedarLeaks` 原子计数器追踪泄漏 goroutine；累计 ≥5 次时联动 KillSwitch Stage 1，使运营可感知并兜底；泄漏 goroutine 最终在 Cedar FFI 返回后自行退出（buffered channel 不阻塞）。Rust 侧 `cedar_evaluate` 已配套引入 timeout_ms 参数以从根本消除阻塞（✅ 已实现）。

**Cedar FFI Failure Mode 表**:
| 失败场景 | 行为 | 审计 |
|---------|------|------|
| Init 失败 | fail-closed + 拒绝启动 (fatal) | CRITICAL |
| Evaluate panic | catch_unwind → deny + WARN | audit + `polaris_cedar_panic_total` Counter |
| Evaluate 超时 (>10ms) | deny + 增加 `polaris_cedar_timeout_total` Counter | WARN |
| 连续 10 次 Evaluate 失败 | KillSwitch Stage 1 THROTTLE | CRITICAL |

### 3.1 Cedar 策略结构

**Agent 能力策略 (permit + conditions)**:
- **read_only**: `trust_level >= 1` → permit
- **write_local**: `trust_level >= 2 AND allowed_paths` 含 `target_path` → permit
- **write_network**: `trust_level >= 3 AND approval == "approved" AND cap_token_valid` → permit
  - *附加 TaintLevel 约束*: `write_network` 工具调用参数中任一 `[TaintLevel] >= [Taint-Medium]` → forbid (需经 SanitizeByUserReview 降至 TaintLow 或 TaintUserReviewed 后方可放行)
  - *附加配额约束 (OWASP LLM06 Excessive Agency 防护)*: Capability Token 必须包含 `MaxCallsPerTask` 维度（如单工具上限 50 次），杜绝无限制死循环代理。

**trust_level 数据来源**（插件/MCP 场景）:
`skill_sources.trust_tier` / `mcp_servers.trust_tier` 在 Cedar 评估上下文注入为 `trust_level`；
值由 `builtinCatalog` 白名单决定，安装时固化，请求方不可覆盖（ADR-0016（Architecture Decision Record，架构决策记录））。
运行时的 `write_network` 等危险操作评估，由 `trust_level` 结合全局 `permission_mode` 共同决断：
- `full_access` 模式：TrustTier ≥ 2 自动通过。
- `auto_review` 模式：TrustTier ≥ 3 自动通过，TrustTier = 2 需 `approval=="approved"`（HITL 补签）。
- `default` 模式：所有外部扩展的危险操作强制需 `approval=="approved"`（HITL 补签）。
- TrustTier = 1 时，所有模式均 deny-by-default，强行阻断。
详见 M13 §8.6 插件安装流程。（**注意**：所有第三方或用户生成的扩展安装必须统一途经 `Manager.InstallExtension`，以确保上述策略被强制下发并执行）。

**硬约束 (forbid 无条件优先)**:
- `deploy/drop_db/delete_data/send_mass_email AND approval != "approved"` → forbid
- `monthly_spend > monthly_budget` → forbid (所有 principal/action/resource)
- `source == "llm_generated" AND capability == "write_network"` → forbid

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

KillSwitch/KillState 三阶段 FSM 实现见 `internal/security/`。

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

要从 FullStop 恢复（进程内活恢复）：
1. 人工审查触发原因和审计日志。
2. 调用 `POST /_admin/unseal`（携带有效 API Key 及 reason）。系统将自动删除 `.fullstop` 文件并恢复运行状态。

封印态持久文件的内容为 JSON：`{"timestamp": <unix>, "reason": "...", "actor": "..."}`

### 4.3 物理触发路径

| 路径 | 机制 | 响应 |
|------|------|------|
| Ctrl+C x3 (3s 窗口) | SIGINT 计数器, 窗口重置归零, >=3 → Full Stop | <1s |
| ~/.polarisagi/polaris/KILLSWITCH 文件 | fsnotify 监视, 存在 → Full Stop | <500ms |
| POST /_admin/kill | 需已认证身份（任意合法用户，非 admin-only）；未配置 `POLARIS_API_KEY` 时按回环 IP (127.0.0.1/::1) 豁免免认证，已配置后不再有 IP 限制 | <100ms |
| POST /_admin/unseal | 强制鉴权且要求 `UserID == "admin"`（不接受匿名/回环豁免，见 `HandleUnseal`） | <100ms |
| [TokenBurnRate] > 10x baseline 30s | 滑动窗口背压熔断 | ~30s |
| Global DoS Guard (LLM10) | 全局信号量饱和 / Session Bucket 耗尽 | 限流或 Stage 1 |

- **TripleCtrlCGuard**: 3s 滑动窗口计数 SIGINT, `归零/>=3` → `executeFullStop`
- **KILLSWITCHFileWatch**: fsnotify 监视 `~/.polarisagi/polaris/KILLSWITCH`, 存在 → `executeFullStop`, 删除后恢复
- **AdminKillEndpoint**（2026-07-24 复核修正，此前文档误述为"仅 127.0.0.1/::1、无认证"，与
  `internal/gateway/server/sysadmin/admin_killswitch.go` `HandleKill` 实际逻辑不符）：
  鉴权规则由 `middleware_auth.go` 统一处理，与其余路由一致——未配置 `POLARIS_API_KEY` 时仅回环 IP
  可匿名访问，配置后任意 IP 携带正确 Key 即可访问；`HandleKill` 本身只检查 `authCtx.UserID != ""`，
  不区分 admin 与普通认证用户，也不做独立 IP 白名单。这是有意的不对称设计（fail-safe）：**触发停止
  的门槛刻意放低**（任何已认证身份都能拉下紧急制动，倾向"宁可错停不可漏停"），**恢复的门槛刻意抬高**
  （`/_admin/unseal` 要求 `UserID == "admin"`，见下一行），二者不应等同看待。POST → `executeFullStop`；
  未认证 → 403。
- **BurnRateFuse**: 订阅 M3 `polaris_token_burn_rate` Gauge (CANONICAL SOURCE) → 当 `EMA_30s > baseline.P95 × 10.0` 持续 30s `[Window-Burst-30s]` → `executeFullStop`。计算逻辑由 M3 单源持有，M11 不独立采样。M3 暴露专用 Counter `polaris_token_burn_stage3_triggered_total`，KillSwitch 从该 Counter 边沿驱动。
- **Global DoS Guard** (OWASP LLM10 Model DoS 防护): 两层遏制——
  1. 全局信号量: 全系统并发 LLM 调用上限（Tier 0=4）
  2. Session Token Bucket: 单任务/会话请求频次约束
  超限 → HTTP 429 / 局部排队；持续强刷 → 晋级 KillSwitch Stage 1 (THROTTLE)。

### 4.4 ESCALATE.md 协议 [ESCALATE]

```yaml
always_escalate: 
  - deploy_to_production
  - send_external_communication
  - financial_transaction
  - delete_data
  - privilege_change
  - cost_exceeds_usd: 100.00
channels:
  slack: "#ai-alerts"
  timeout: 10min
  email: "ai-ops@company.com"
  timeout: 30min
approval:
  timeout: "见 spec/state.yaml §m11_policy.escalation_timeout_minutes"
  on_timeout: escalate_to_killswitch (Stage 3)
  on_denial: halt_and_log
  on_approval: proceed_and_log
```

---

## 5. 数据隐私与凭证安全

### 5.1 PII 检测与红化 [PIIGuard]

**PIIGuard**:
- **组件**: PIIDetector (Go 原生正则 + 规则引擎 + 可选 Presidio sidecar) + PIIDesensitizer + PIITokenVault
- **Tier 0**: Go 原生正则 (<1ms)，覆盖 P0 结构化模式（信用卡/SSN/API（Application Programming Interface，应用程序接口） Key/邮箱/手机/IP）。
- **Tier 1+**: 显式启用 Presidio sidecar，高精度 NER。`FeatureGate.FeaturePresidioPII` 自动化（≥Tier1 且 ≥512MB free→启用），详见 M03 §5。

**Tier 0 降级行为契约**: 仅保证结构化 PII 检测；非结构化 PII（姓名/地址/出生日期/雇主/医疗/生物特征/行为画像/家庭关系）不保护，会进 LLM prompt。
首次进入 PII 场景（开启 Notion/Gmail Connector 等）主动告警："Tier 0 仅基础防护，建议升级 Tier 1+ 启用 Presidio"。

**RedactMode**:
- **RedactBlock**: 含 PII → `ErrPIIDetected`, 阻止执行
- **RedactReplace/SessionTokenizer/OpaqueToken**: ✅ 已实现（PIIDesensitizer 格式保留假数据 + PIITokenVault 会话级可逆令牌，二者分工不同，互不替代。PIITokenVault 和崩溃恢复用途的 SessionPIIVault 各自独立）
- **RedactWarn**: 含 PII → warn 日志继续

**PIIGuard 双向防护**: PIIGuard 同时在输入端（M4→M7 工具参数 SecureUnredact 之前）和输出端（M7 ToolResult→EventLog PostExecution Redact，M7 §4.3 Step 5）工作。输入端阻止 PII 进入 LLM Provider，输出端阻止 PII 进入不可变审计轨迹。Tier 0 仅保证结构化 PII 模式检测覆盖两端。

**SessionPIIVault**（实现：`internal/agent/context/pii_vault.go`）:
- `Snapshot(ctx, taskID, fields map[string]string)` → 逐字段 AES-256-GCM 加密写入 `preferences` 表（key=`pii_vault:{taskID}:{field}`，TTL=1h）
- `RestoreFromSnapshot(ctx, taskID)` → 读 preferences 表解密，写回 WorkingMemory.Scratch
- `SecureZero(ctx, taskID)` → `DELETE FROM preferences WHERE key LIKE 'pii_vault:{taskID}:%'`

  当前边界：PII 字段直接存入 preferences 表（持久化），TTL 1h 自动过期。
  **文档纠正（原"VFS blob 路径存储已实现"为失实表述，已核实并移除死代码）**：此前本节声称 `internal/vfs/provider.go` 的 `BlobStore`（content-addressed `vfs://<hash>` blob 存储）"已作为通用基础设施完整实现"——经代码核实，该接口自始至终没有任何 producer 实现（`vfs.WorkspaceManager` 只有路径寻址的 `WriteFile`/`ReadFile`，没有 `WriteBlob`/`ReadBlob`），且全仓库零消费方，属于纯文档漂移（接口写了、代码从未做）。已删除 `protocol.BlobStore`/`COWProvider`/`VFSFacade`（`internal/protocol/interfaces_vfs.go`）与重复的 `memory.VFSProvider` 定义，不臆造实现（CLAUDE.md 禁止超前抽象）。MutationBus 落盘（`internal/protocol/schema/002_outbox.sql` + M2 §2.3 DatabaseWriter）核实属实、确已实现，与本条无关，PII 侧目前也未走这条路径。PII 字段当前用 AES-256-GCM 加密直接落 `preferences` 表 + TTL 1h 自动过期，规模和生命周期均可控，暂无 blob 存储的现实需求；若未来出现真实需求（如需要存储大体积 PII 相关附件）再按需设计，不预先搭好用不到的接口。

  **OpaqueToken 与 SessionPIIVault 的语义边界**：二者是强度不同的两套方案，不能互相等价代替：
  - **OpaqueToken**（把 PII 在进入 LLM prompt 前替换为占位符 token、模型只见占位符、事后按需把占位符换回原文、原文全程不落盘）——✅ **已完全闭环实现**。
    - **令牌化（输入端）**：`internal/agent/agent_execute.go` 的 `executeEffect` 入口调用 `withTaskScopeCtx` 把 `a.sCtx.SessionID`（不是 `a.sCtx.TaskID`——二者是不同字段，SessionID 贯穿会话生命周期不变，TaskID 随认领的 Blackboard 任务变化）注入 `ctx.Value(protocol.CtxTaskIDKey{})`；主路径和 PRM 候选路径组装好 `types.Message` 之后、调用 `provider.Infer` 之前，通过 `tokenizeMessagesForLLM` 对每条消息 `Content` 做 PII 提取和令牌化。任何提取错误均按 fail-closed 策略阻断，防止敏感信息流出。
    - **隔离与清理**：`guard.PIITokenVault` 内部为 `map[SessionID]map[token]真值` 二维结构，`TokenizeForTask`/`ResolveForTask`/`RestoreForTask` 均严格按 SessionID 命名空间隔离，**不做跨命名空间回退查找**——用错误的 SessionID 还原会 fail-closed 拒绝，而不是静默从其它会话的桶里读到值。`agent.go` 的 `handleTerminalState`（终态触发，`Run()` 即将返回前）调用 `ClearTask(a.sCtx.SessionID)`，与 `SecureZero` 协同执行，仅清理当前会话自己的命名空间，不影响进程内其它并发会话，避免内存泄漏。
    - **还原（输出端）**：在 `internal/tool/tool.go` 的 `InMemoryToolRegistry.ExecuteTool` 内，通过 `ctx.Value(protocol.CtxTaskIDKey{})` 提取同一 SessionID，并使用 `RestoreForTask` 安全精准还原真值，用后即焚。该 ctx 值与 `dag/executor.go` `DAGExecutor.Execute(ctx, plan, a.sCtx.SessionID, a.sCtx.AgentID)` 沿用同一仓库既有惯例，保证令牌化端与还原端使用同一 taskID 命名空间。
    - **已知局限**：目前只会针对 `Message.Content` 进行令牌化保护；`Message.Parts` 中因可能夹杂极其复杂多态的结构与多模态数据，强制文本替换具有高风险性，因此暂不纳入自动令牌化保护层。
  - **`SessionPIIVault.RestoreFromSnapshot`**（`internal/agent/context/pii_vault.go`）解决的是另一个更弱隔离级别的问题：`Snapshot` 把字段**原文**（非占位符）AES-256-GCM 加密写入 `preferences` 表，`RestoreFromSnapshot` 解密后写回 `WorkingMemory.Scratch`；唯一调用点是 `internal/agent/recovery.go` 的 Agent 崩溃恢复（Suspended→Resume），本质是"同会话内原文加密落盘 + 按 taskID 解密回填"，不涉及跨调用边界的占位符替换/换回，也不阻止原文进入 LLM prompt。

### 5.2 Credential Vault [CredentialVault] 【已实现】

> 架构变更（2026-07-03）：为了彻底解决 Headless、Docker 等无 GUI 环境的兼容性问题，废除了原定的 OS Keychain 复杂抽象，统一采用基于文件/环境变量的主密钥 AES-256-GCM 加密方案。

实现机制 (`internal/security/credential/vault.go`):
  - **透明加解密**：`SQLiteProviderRepository` 在读写 `providers` 表（api_key 列）时，通过依赖注入的 Vault 实例自动完成 `Encrypt`/`Decrypt`。
  - **Ciphertext 识别**：密文统一附加 `v1:` 前缀，提供对旧版明文的后向兼容与无缝迁移能力。
  - **接口契约**：不再对外暴露原生 Get/Set/Delete 密钥接口，而是提供 `Encrypt(plaintext)` 和 `Decrypt(cryptoText)` 原语。

**persistent_key 轮换**:
- 触发: `polaris vault rotate-master-key`
- 流程: 读取旧密钥初始化 OldVault，生成新密钥初始化 NewVault；遍历 `providers` 表执行 `Decrypt(old) -> Encrypt(new)` 并更新；最后原子替换 `vault.key` 文件。

**冷启动主密钥（Master Key）决策树**:
  1. 优先读取 `POLARIS_VAULT_PASSPHRASE` 环境变量（SHA-256 派生 32 字节）。
  2. 其次读取 `~/.polarisagi/polaris/vault.key`（0600 权限）。
  3. 如果均不存在，自动生成高熵随机密钥并存入 `vault.key`。

### 5.3 local_only 网络沙箱三层防御

双层隔离:
  - OS 级沙箱（`internal/security/`）: macOS sandbox-exec（已实现）/ Linux Landlock LSM（内核 >=5.13，不可用 fail-closed）/ Windows（`internal/security/network/local_only_windows.go` 已用 `netsh advfirewall` 添加出站阻断规则实现网络隔离层）。**注意区分**：D3 沙箱分级层（`internal/sandbox/native_os_sandbox.go`）的 Windows WSL2 支持仍未实现，此处特指 M11 network local_only 网络隔离层，两者是不同子系统。
  - Go 层纵深: RoundTripper 替换 no-op transport + DefaultResolver 覆写 NXDOMAIN + Dialer.Control 拒非 loopback IP
启动期自检: DNS 解析公网域名 → 有响应 → 拒绝启动 (fail-closed)。

Tier 3 本地模型守卫: M1 LocalProvider.Probe() 验证可加载模型且峰值 RSS + 已用内存 < 64GB (1GB 预留)，否则拒绝 local_only。

**当前实现状态：已实现（2026-07-03）。** `protocol.LocalProvider` 接口新增 `Probe(ctx) (LocalProbeResult, error)`（`internal/protocol/interfaces.go`），只读校验当前是否有模型处于已加载可用状态，不触发加载；`LocalAdapter.Probe()`（`internal/llm/adapter/local.go`）实现该方法，复用已有的 `LocalStatus()` + 新增的 `probe.ProcessPeakRSSBytes()`（`internal/observability/probe/process_rss_{linux,darwin,windows}.go`，getrusage RUSAGE_SELF 读取 ru_maxrss）与 `probe.MemoryProbe()` 汇总系统已用内存。`NetworkSandbox`（`internal/security/network/local_only.go`）新增 `SetLocalProvider()` 注入点（与既有 `SetSafeDialer()` 同构），`StartupCheck()` 在原有 60GB 物理内存门槛检查之后追加第 5 步：调用 `LocalProvider.Probe()`，若模型未加载或 `峰值RSS+已用内存 >= 64GB-1GB` 则 fail-closed 拒绝进入 local_only。60GB 物理内存检查校验硬件容量，Probe() 校验运行时实际预算，两者互补（前者通过不代表后者一定通过——其它进程可能已占用大量内存）。单元测试见 `internal/llm/adapter/local_test.go`（`TestLocalAdapter_ProbeGraceful`）与 `internal/security/network/local_only_test.go`。

**当前实现状态：已接入（2026-07-06 前已完成，随并发稳定性修复一并确认）。** `NetworkSandbox` 构造 + `Enable()` + `StartupCheck()` 调用链已接入 `cmd/polaris/boot_server.go`（`bootServer()` 函数开头）：读取 `sb.Cfg.Security.LocalOnlyMode`（`internal/config/config.go` `local_only_mode`），为真时依次执行 `network.NewNetworkSandbox(100)` → `SetSafeDialer()` → `SetLocalProvider()`（2026-07-04 审计补齐，此前从未调用，导致 Tier3 本地模型内存预算守卫被静默跳过）→ `Enable()` → `StartupCheck()`；`Enable()` 必须先于 `StartupCheck()` 执行（若顺序颠倒，loopback-only 探测会在防御生效前进行，导致 `local_only_mode=true` 时启动 100% 失败，2026-07-04 审计已修正此顺序）。任一步失败均 `apperr.Wrap` 返回错误阻止启动（fail-closed）。至此 local_only 模式已端到端可用，不再是遗留缺口。

白名单 `local_only_network_allowlist.toml` (用户 Ed25519 签名): 仅 M10 Connector 豁免，Tier 3 上限 5 条，变更需重启。Rust FFI 引擎 (SurrealDB-Core/Cedar) 无网络能力；Tier 0/1/2 彻底禁用 local_only，不进行加载。

---

## 6. SSRF 与 DNS Rebinding 防护 [SSRFGuard]

blockedCIDRs（`init()` 预编译）：0.0.0.0/8 / 127.0.0.0/8 / 10.0.0.0/8 / 100.64.0.0/10（CGNAT）/ 172.16.0.0/12 / 192.168.0.0/16 / 169.254.0.0/16 / ::1/128 / fc00::/7 / fe80::/10。dnsCache TTL 见 `spec/state.yaml §m11_policy.safe_dialer_dns_cache_ttl_seconds`。

**五阶段**: 
- **Phase 0** — Capability 出口强制 (read_only 禁止写 HTTP / write_local 仅内网) 
- **Phase 1**: DNS 解析 
- **Phase 2**: blockedCIDRs 校验 
- **Phase 3**: TOCTOU 延迟（`spec/state.yaml §m11_policy.safe_dialer_toctou_delay_ms`）后二次 DNS 解析 + blockedCIDRs 校验 
- **Phase 3.5**: 响应 IP 超阈值（`spec/state.yaml §m11_policy.safe_dialer_max_ips_threshold`）→ 拒绝 
- **Phase 4**: DNS TOCTOU 消除（验证通过后覆写 DialContext 锁定 IP，Request.Host 保留原始 hostname）。

与 M7 协作: M7 做 URL/IP 静态校验 + Capability 声明层收缩，M11 做出口强制执行 + DNS Rebinding 动态检测 + IP 锁定。两层纵深防御: 声明层(M7) → 网络出口层(M11 Phase 0)。

**统一安全 Dialer** (`internal/protocol/interfaces.go` SafeDialer):
- M11 导出 `SafeDialer.DialContext`。四层注入覆盖全出站: `http.Transport.DialContext` / `grpc.WithContextDialer` / `websocket.Dialer.NetDialContext` / `net.DefaultDialer.Control`
- DialContext 内执行五阶段 SSRF (Phase 0-4)。
- **Taint 出口拦截**: 调用方在 DialContext 前显式调用 `SafeDialer.TaintEgressCheck(taintLevels)`，`[Taint-Medium]` 及以上级别（TaintMedium/TaintHigh）未经 SanitizeByUserReview → `ErrTaintBlockedEgress`。Gate.TaintEgressCheck 与 SafeDialer.TaintEgressCheck 采用同一阈值（`>= TaintMedium`），两层一致防止出口绕过。
- **两层纵深**: M7 Policy Gate4（声明层预检）+ M11 SafeDialer.TaintEgressCheck（出口层终检，调用方职责）。
- M7/M10/M13 所有出站必经此入口。CI `safe_dialer_lint` 扫描裸 `net.Dial` / `grpc.Dial` / `http.Get` → ERROR。
- **✅ 已修复（SurrealStore 出口缺失）**：Go 侧 SurrealStore wrapper 在调用 `surreal_store_insert` / `surreal_store_query` FFI 前补充 `SafeDialer.TaintEgressCheck`，确保 TaintHigh 数据不绕过出口拦截直写认知存储。

**读写能力分级出口检查**（`internal/security/network/safe_dialer_capability.go`）：Phase 0 声明层检查之外，另有一层 HTTP 方法级的能力校验，与 Phase 0 的粗粒度 `read_only`/`write_local` 声明是两个层次——Phase 0 拦在 Capability Token 声明侧，本层拦在实际出站 `http.RoundTripper` 侧，纵深防御互不替代。三级：`CapNetworkRead`（仅放行 GET/HEAD/OPTIONS，其余方法 → `ErrCapabilityWriteBlocked`）/ `CapNetworkWriteLocal`（放行，内网 IP 校验由调用方在 DialContext 中执行）/ `CapNetworkWrite`（放行，交由 Phase 1-4 保护）。通过 `WrapCapability(inner, cap)` 包装现有 `http.RoundTripper` 接入（`CapabilityRoundTripper.RoundTrip` 在转发前调用 `CheckCapability`），消费方：`internal/tool/builtin/fetch_url/`、`internal/tool/builtin/web_search/`（2026-07-14 补齐，此前用裸 `http.Transport` 完全绕过该层校验，是纵深防御缺口）。

---

## 6.5 D6 防线：`[FactualityGuard]` 输出真实性核验

> **inv_global_06**: 与 PII Guard 并列守护 LLM 输出边界。D1~D5 守护输入与权限，D6 守护**输出事实性**。

**实现**: `internal/security/guard/factuality_guard.go`

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

跨模块传输污点数据时（`internal/security/`），Seal 附加 HMAC-SHA256（覆盖内容 + 污点等级 + 来源实体 ID），Unseal 时重新计算并比对；HMAC 不匹配则强制将污点升级到 TaintHigh，防止反序列化路径绕过污点标记（降级攻击）。

---

## 7. 不可变审计轨迹

**实现**: `internal/security/`（AuditTrail）

### 7.1 Append-Only Hash Chain

每条 `AuditRecord` 包含：事件 ID / Unix μs 时间戳 / Agent ID / Session ID / 操作类型 + 详情 / 信任等级 / 授权来源 / 操作结果（allow/deny/error/escalated）/ 拒绝原因 / PII 标志 / PrevHash / RecordHash。

Hash Chain 结构：`RecordHash = SHA-256(序列化后记录，不含 RecordHash 字段本身)`，`PrevHash(i) = RecordHash(i-1)`，首条 PrevHash 为空字符串。所有记录持久化到 `events` 表（topic='audit.policy'），DDL 层 trigger 禁止 UPDATE/DELETE（append-only 强制）。

`VerifyIntegrity()` 遍历内存链逐条重算 RecordHash 并比对 PrevHash 链接，返回 (ok bool, brokenIndex int)。同时在 `RecoverOnStartup()` 中对从 DB 恢复的尾部 100 条记录执行完整性校验，不通过则拒绝启动。

**DB 层 Hash Chain（全事件覆盖）**：`events` 表新增 `hash`/`prev_hash` 列，由 `DatabaseWriter.executeInsertEvent` 在 INSERT 时同步计算：`hash = SHA-256(id||topic||actor||type||payload||prev_hash)`。提供独立于内存审计链的持久可验证层，覆盖全表事件（不限 audit.policy topic）。两条链的关系：内存审计链守护 audit.policy 事件实时可验证；DB 层 hash chain 是完整性备份，崩溃恢复后可验证全量历史事件。

### 7.2 Epoch 轮转

触发：审计日志估算体积 > 100MB（由调用方传入当前 MB 数）。封存流程：追加 `epoch_end` 标记记录（FinalHash + RecordCount），写 DB；更新 epochID；追加 `epoch_start` 标记（PrevEpochFinalHash），建立跨 Epoch 密码学连续性。归档目录 `~/.polarisagi/polaris/audit/archive/`，保留 90 天（Tier 0）。

### 7.3 Outbox Worker 增量消费（HE-Rule-6）

`internal/store/`（OutboxWorker）实现主循环：游标持久化到 `sys_config` 表，重启后从 DB 恢复防止漏消费；每批处理后原子 CAS（Compare-And-Swap，比较并交换） 更新游标（Exactly-Once 语义）；失败记录指数退避，连续崩溃 ≥3 次标记 dead（毒丸清除）；ReplayMode 时跳过所有副作用。

---

## 8. 多 Agent 宪法分层

Layer 4: Agent 间交互规则 (在 §1 三层宪法之上):
  - Agent 间信息传递边界
  - 任务委托链深度上限
  - 跨 Agent 权限组合约束
  - 黑板消息最小权限路由

**Cedar 策略扩展 (Layer 4)**:
- `forbid send_message` → `BlackboardEvent: payload` 含 `cross_agent_prohibited_data AND principal.id != source_agent_id`
- `forbid delegate_task`: `delegation_chain_depth >= 3`
- `forbid call_tool`: `capability == "write_network" AND` 任一 collaborating_agent `trust_level < 3`

**JIT Token 委托能力收缩**：协议层（`internal/security/`）负责能力取交集、TTL 减半衰减，携带父 Token 溯源链；业务层（`internal/action/`）负责深度≥3 拦截和有效能力计算；验证层（`internal/security/`）负责子集校验、过期约束、沙箱层级不降级三条守卫。能力取交为空分别向业务层和协议层返回不同错误码。

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
| M10 Knowledge RAG（Retrieval-Augmented Generation，检索增强生成） | Connector Taint 初始等级查找表 | M10 §0, M11 §2.4 |
| 全局字典 | TaintLevel/Taint-Prop/Taint-Sanitizer/KillSwitch 完整定义 | 00-Global-Dictionary §4, §5, §8 |
| DDL | 001_events（审计轨迹 source）、006_decision_log（决策日志） | internal/protocol/schema/ |
| 时序图 | KillSwitch 触发与响应链全流程 | DIAGRAMS.md#killswitch |
