
# 模块 6: Skill Library

> 可命名、可参数化、可索引的复用技能。Go 主导管理+检索+Logic Collapse，script runtime（Python，ContainerSandbox 执行）。[HE-Rule-3] [HE-Rule-5]
<!-- §跳读: 0-bis:6 职责 / 0-ter:16 不变量速查 / 1:27 技能表征 / 2:46 生命周期(CANONICAL) / 3:123 检索系统 / 4:149 演化 / 5:174 282 脚本缓存 / 7:198 317 (SOFT)降级 / 8:212 331 依赖 / 9:225 344 AgentSkills标准格式适配 -->
## 0-bis. 职责边界

- M6 **是**: 技能注册、索引、检索、生命周期管理 | **不是**: 技能沙箱执行（M7）
- M6 **是**: Logic Collapse 蒸馏（System 2 轨迹 → Python 脚本 → ContainerSandbox 执行） | **不是**: LLM 推理调用（M1）
- M6 **是**: SkillSelector 启发式匹配（不调 LLM） | **不是**: Agent 状态机控制（M4）
- M6 **是**: HMAC-SHA256 签名验证（加载前置；本地离线签名，非 cosign） | **不是**: 签名策略制定（M11 Cedar-Gate）
- M6 **是**: SKILL.md（script runtime）或 SKILL.md + src/skill.py（Python runtime，Logic Collapse 生成）技能管理 | **不是**: 工具注册与发现（M7 ToolRegistry）

---

## 0-ter. 不变量速查表

- 编号: inv_M6_01 | 不变量: SkillSelector 不调 LLM——启发式 + 向量匹配 + 排序公式，保持确定性 | 验证方式: 代码审计 + CI（Continuous Integration，持续集成） `skill_selector_llm_free`
- 编号: inv_M6_02 | 不变量: Logic Collapse 产物必经 staging 7 阶段——禁止 M9 直写 skills 表 | 验证方式: M9 → M2 Outbox 路径审计
- 编号: inv_M6_03 | 不变量: risk_level 缺失/歧义时默认最高风险——SandboxTier 取最严格级别 | 验证方式: M11 PolicyGate 代码审计
- 编号: inv_M6_04 | 不变量: 仅幂等技能可缓存结果——非幂等技能每次重新执行 | 验证方式: SkillCache key 包含 idempotency 标记
- 编号: inv_M6_05 | 不变量: HMAC-SHA256 签名校验失败 → fail-closed 拒绝加载 + CRITICAL 审计 | 验证方式: M6 §2 验证流水线 Step 4
- 编号: inv_M6_06 | 不变量: 编译前安全闸门全部满足才触发 Logic Collapse——成功次数/语义方差/Eval Gate | 验证方式: M6 §2.2 编译前安全闸门

---

## 1. 技能表征

### 1.1 目录结构

- **script runtime**（SKILL.md 指令，LLM 执行）：`skill-name/SKILL.md`（NL 描述 + 使用指令，全文存入 `skills.instructions`）+ 可选 `agents/polaris.yaml`（display_name/policy/dependencies）
- **Python runtime**（Logic Collapse 生成，ContainerSandbox 执行）：`SKILL.md` + `schema.json`（入参/出参 JSON Schema）+ `src/skill.py`（蒸馏产物，`def execute(input: dict) -> dict:`）+ `test/`（Eval Harness 输入）+ `SIGNATURE`（HMAC-SHA256）

蒸馏产物经 ContainerSandbox（L3，复用 CodeAct 执行路径）执行，与插件市场 Python 插件格式一致，可直接发布市场。

### 1.2 数据结构

Skill/JSONSchema/Condition/SkillSource 定义见 `pkg/types/models_skill.go`，接口见 `internal/protocol/interfaces_skill.go`。

- **依赖环检测（已实现）**：`SkillMeta.DependsOn []string` + `ComposesOf []string`（`008_skills.sql` 同步增列）；`SQLiteRegistryImpl.Register()` 与内存 `RegistryImpl.Register()` 均在 INSERT 前 BFS 环检测，循环依赖拒绝注册。
- **版本更新（部分实现）**：子技能 Version++ → 反向依赖 BFS 扫描 → `UPDATE skills SET needs_compat_check=1` 已实现（`internal/extension/skill/sqlite_registry.go` `markReverseDependenciesCompatCheck`，内存版 `skill.go` 对齐）。打标后不会自动触发半隔离沙箱集成重测，需人工或后续流程消费该标记。
- **版本约束**：`"skill-id@v1"` 锁主版本（允许 1.x 补丁）；`@v1.2` 锁次版本；`@v1.2.3` 锁精确版本。Patch 递增不触发父技能兼容检查；Minor/Major 触发。

---

## 2. 技能生命周期

### 2.1 五阶段流水线

1. **轨迹记录**：Agent 成功完成多步任务后，完整执行轨迹（LLM 调用、工具调用、环境上下文）持久化到 EventLog
2. **非确定性剥离**：LLM 具体决策提取为硬编码确定性参数，丢弃推理链；环境上下文平台无关化（绝对路径 → `{workspace}`、时间戳 → 相对时间、主机名/用户名/IP 移除）；仅保留确定性工具调用序列（基于 L1 原生工具集如 `str_replace_editor`、`glob`）
3. **参数化抽象**：识别可变参数（路径 → `{input_path}`、搜索词 → `{search_pattern}`、数值 → `{threshold}`），推断类型生成 InputSchema/OutputSchema，提取默认值、标记 required
4. **契约推导**：默认用 DeepSeek V4 推导前置条件（环境/工具/权限）、后置条件（验证标准）、风险等级和沙箱 tier
5. **脚本生成 + 索引 + 签名**：LLM 生成 `src/skill.py` → 静态分析 → ContainerSandbox 行为测试 → HMAC-SHA256 签名 → 写入 Skill Index → SurrealDB-Core KV 缓存

### 2.2 Logic Collapse: System 2 → System 1

> `FeatureLogicCollapse` 门控（Tier 0+，≥8GB 且 ≥1GB free 自动启用，见 M03 §5.4）。蒸馏流水线：新鲜度检查 → 数据脱敏 → 编译门控 → LLM 代码生成（Python）→ 静态分析（ValidatePython）→ ContainerSandbox 行为测试 → 风险分级 → HMAC-SHA256 签名 → Skill 注册。M9 触发器位于 `internal/learning/`（在线语义方差估算 + HITL 分流）。<8GB VPS 仅加载预生成技能。决策见 ADR-0008。

流程：System 2 成功执行 → 轨迹分析 → LLM 代码生成 → ValidatePython 静态检查 → ContainerSandbox 探针测试 → 签名入库 → System 1：同类任务 `SurpriseIndex < 0.3` 时 Python 技能直接命中，零 LLM 推理 `[SurpriseIndex]`。

**蒸馏策略**：技能先以 SKILL.md 形态存在，累积 >= 50 次成功 + 语义方差检查 + HITL/Eval Gate → Logic Collapse → Python 脚本。

**local_only**：脚本生成依赖 LLM API，local_only / Tier 0 内存不足时 Logic Collapse 禁用，仅加载预生成技能（`ErrLogicCollapseDisabled`，降级 SKILL.md 模式 + WARN）；由 `FeatureGate.FeatureLogicCollapse` 自动判定，调用方无需手写分支。用户首次进入 local_only 时主动提示能力降级。Tier 0 预生成技能库随版本发布，覆盖 System 1 核心能力面。

**编译前安全闸门**（全部满足才触发）:
1. 成功次数 >= 50（不足 → `ErrInsufficientSuccessCount`）
2. SemanticVarianceCheck：50 次成功轨迹输入 embedding 方差 < 0.1 → 拒绝，标记 `needs_more_diversity`
3. HITL/Eval Gate（未通过 → `ErrEvalGateNotPassed`）：
   - RiskLevel >= "high" → HITL Gateway [ESCALATE]；< "high" → M12 Eval Harness 自动回归
   - Day-0 冷启动分级阈值（`minEvalCasesPerSkill=5`）：(a) 黄金用例=0 且成功≥50 → Auto-Eval-Bootstrapping：M12 放开抽样池，DeepSeek V4 批量审查（抽取最分散的约 20 条轨迹）交叉审查越权/数据泄漏/行为偏差，全过 → `source=synthetic_auto_bootstrap`，任一未过 → `needs_review`；(b) 0<用例<5 → 阈值降至实际用例数，通过 → `eval_coverage_partial`；(c) 用例=0 且成功<50 → `ErrInsufficientSuccessCount`

**生成前静态分析**（LLM 生成 `src/skill.py` 后、沙箱执行前）：当前实现 `internal/extension/skill/compile.go` `ValidatePython`（文本匹配禁止 `import os` / `subprocess` / `socket` / `eval(` / `exec(`，允许 `json` / `re` / `math` / `datetime`）。失败 → 拒绝生成，轨迹进入 MEMF + 写 `skill_static_analysis_rejected` 审计。计划（Tier 1+）升级为 Python AST 分析：控制流图（不可达代码、时间炸弹）、危险 import（`os`/`subprocess`/`socket`/`ctypes`）、非确定性调用（`time.time()`/`random`/`os.environ`）。

**脚本方案**：LLM 生成 Python 经 ContainerSandbox（L3）执行（零增量依赖，ADR-0008）；依赖 `FeatureLogicCollapse` AND `FeatureL3Sandbox` 双门控，L3 不可用时跳过蒸馏（仅存元数据）。

**并发控制**（同时约束 Logic Collapse 编译路径与 SkillIndex lazy JIT 编译路径 Silver/Bronze）:

| Tier | 并发编译数 |
|------|-----------|
| <8GB VPS | Logic Collapse 不可用，仅预生成技能 |
| Tier 0 (8GB) | 1（限制编译期内存峰值） |
| Tier 1+ (16GB+) | 2 |
| Tier 2+ (24GB+) | 4 |

JIT 编译阻塞等待 ≤5s，超时 fallthrough → SKILL.md LLM 执行。CompileGate 准入：空闲内存 >= 80MB（50MB 技能预算 + 30MB 安全边距）[Tier-0-Limit]。**编译主流程**：① privacyMode=="local_only" → 降级 SKILL.md ② compileGate() 内存检查 ③ canStartNewCompile() 并发检查 → 编译。

**沙箱路径映射**: `/workspace/` → M2 Workspace 当前 task 目录；`/tmp/sandbox/` → `os.TempDir()/polaris_sandbox_{skill_id}/`；宿主绝对路径沙箱内不可见，技能经 M7 Workspace Bridge 按需取文件内容。

**脚本数据安全**（`buildToolCallsDescription`，`internal/learning/logic_collapse_codegen.go`）：纯类型签名剥离——仅把工具调用类型签名 + Input/Output Schema 交给 LLM，从源头剥离具体参数值，PII 不进入 LLM 生成路径。

**脚本 ABI（stdin/stdout JSON）**: 宿主将参数 JSON 写入进程 stdin；脚本 `execute(input: dict) -> dict` 返回结果字典；宿主读 stdout → JSON 反序列化 → ToolResult。原始参数值由宿主在调用时直接经 stdin 注入（不经 LLM 路径，无需还原映射）。每个技能仅接收自身声明的参数（最小权限），禁止全局 PII 访问接口。

**Trace Data Stripping**（LLM 代码生成前）：LLM 仅接收 (a) 工具调用序列类型签名（不含参数值）、(b) 成功/失败状态、(c) 执行顺序 DAG 拓扑；参数值仅保留类型 + 长度/大小元数据；不可逆 strip-only；先于 AST 脱敏执行。

**Freshness Check**（源轨迹时效性）：触发前验证关键决策是否被后续 Semantic Memory 更新 supersede。500ms 超时，O(N*M)（N=toolCalls, M=每调用实体数）：遍历工具调用 → 实体/关系 UpdatedAt > trace.CompletedAt → 标记 StaleEntity/StaleRelation；不 Fresh → markTraceAsStale。失败不阻塞系统，轨迹标 `needs_adaptation` 等待 M9 下轮评估。

**Logic Collapse 调用顺序**: freshnessCheck → dataStripping → compileGate → canStartNewCompile → LLM 代码生成 → AST 脱敏 → 远程编译。

**命名空间规则**：技能与 Built-in 工具命名空间物理隔离——`skill:` 前缀归 SkillLibrary（Built-in + Logic Collapse + 用户自定义，如 `skill:my_custom_skill`），`tool:` 前缀归 M7 ToolRegistry Built-in 原语（如 `tool:glob`）。`SkillLibrary.Register` 强制 `skill:` 前缀；M7 `ToolRegistry.Register` 强制 `tool:`（或无前缀，向后兼容）。M4 RouteReasoning 在 System 1 命中阶段分别查找，不存在跨命名空间同名覆盖。

**并发同名冲突**：M9 BackgroundTaskScheduler 允许多个 Logic Collapse 并发排队。`SkillLibrary.Register` 写 skills 表前 `SELECT skill_id, semantic_cluster_id FROM skills WHERE skill_id = ?`：
1. 已有同名且 `semantic_cluster_id` 不同 → 以 `candidate_emit` 时间戳较晚者为准（后入覆盖），写 `skill_name_collision` 审计（含两次 emit 的 semantic_cluster_id、聚类中心距离）
2. `semantic_cluster_id` 相同（同语义重复提交）→ `version++` 正常更新，不视为冲突
3. 覆盖后 M4 DAGNode 对旧版本的引用不受影响（引用锁定 `skill:X@v{N}`，新版本 `@v{N+1}`），遵循 §1.2 版本约束

**存储物理路径**：`SkillRegistry.Register`（`internal/extension/skill/sqlite_registry.go`）直写 SQLite `skills` 表（`ON CONFLICT(name) DO UPDATE` 幂等 upsert，元数据 + `instructions` 全文）；SKILL.md + src/ + test/ 为文件系统 Ground Truth（Git 版本控制）[Storage-SQLite]。不存在 `SkillCreatedEvent` 事件与 outbox 脚本 blob 投影链路。

**技能创建后演化**：见 §4.1（`SkillEvolutionMonitor`）。**OpenClaw 迁移**：`polaris migrate openclaw --preset=skills` 把 OpenClaw 的 SKILL.md 拷到 workspace/skills/，可直接以 script runtime 运行，累积足够轨迹后可 Logic Collapse（M13 §1.1）。

### 2.3 技能验证流水线

编译时四阶段（`internal/extension/skill/`）：
- **Step 0（前置）Taint-Check** [Taint-Medium] [Taint-Floor-Medium]：TaintMedium+ 轨迹在编译流水线最前端拒绝（早于 LLM 代码生成），防污染数据驱动代码生成
- **Step 1 静态分析（AST syscall 审计）**：禁止 `os/exec`、`net/http`、`unsafe` 等导入；禁止 `time.Now`/`rand.Read`/`os.Getenv` 等非确定性调用；检测时间炸弹（条件性时间激活）
- **Step 2 脚本沙箱行为测试**：对 `CompileRequest.TestCases` 逐用例 ContainerSandbox 验证（`containerSandbox` 为 nil 仅做静态检查）；Python 语法验证（`python3 -m py_compile`）在此执行
- **Step 3 风险分级 + 签名入库**：按 Python import 声明评估风险（low/medium/high）与 SandboxTier；HMAC-SHA256 签名（本地 key，非 cosign）；写入 SkillRegistry

fuzz 测试（10,000 随机输入）由 `make fuzz-skill` 驱动，见 `internal/extension/skill/fuzz_test.go`。

---

## 3. 技能检索系统

### 3.1 三级检索（已废弃，保留为演进参考）

`internal/extension/skill/retriever.go` 的 `HybridRetriever`（实现 `protocol.SkillSelector`，标注 Deprecated，`boot_tools.go` 不再实例化）设计三路并行检索，与 `internal/memory/retrieval` 独立：
- **L1 vecIndex**：embedding → top-N 语义检索 [Storage-SurrealDB-Core]
- **L2 sigMatcher**：任务特征哈希 → 行为相似技能（IntentSignature + ExecutionSignature）
- **L3 depGraph**：PPR 遍历技能依赖图（alpha=0.6，depth=2）

加权融合 + 上下文预算渐进披露（name+desc → workflow summary → full instructions）。降级链：embedding 维度变化切 FTS5/BM25 + Lazy Re-embedding；`cognitive`/`embedFn` 不可用整体降级为 `SQLiteRegistryImpl` 内存二次过滤（`fallbackSelect`）。

**当前技能发现由 `search_tools` 统一承担**：技能以 ToolSkill 源进入 CompositeCatalog，`search_tools` 向量检索动态激活。

### 3.2 结构签名匹配

- **IntentSignature（路由预检级）**：M4 在 S_PLAN 前使用——目标描述哈希、输入/输出类型、领域提示，判断是否命中 System 1 缓存。匹配度 ≥ 0.8
- **ExecutionSignature（缓存替换级）**：DAG 编译后——工具调用序列哈希 + DAG 拓扑哈希（节点数 + 边集 + 并行度），精确去重。匹配度 ≥ 0.95

IntentSignature 粗筛、ExecutionSignature 精筛，最终按成功率排序返回 topK。

### 3.3 PPR 依赖遍历

依赖图（DependsOn + ComposesOf）经 Personalized PageRank 遍历。DependsOn 双向遍历；ComposesOf 仅向上（子→父），避免检索膨胀。BFS 收集候选后 PPR 以 alpha=0.6（60% 随机游走、40% 跳回种子）算分，降序排序。

---

## 4. 技能演化

### 4.1 演化触发（SkillEvolutionMonitor）

`internal/extension/skill/skill_evolution.go` 的 `SkillEvolutionMonitor.CheckAndEvolve`：由 ConsolidationPipeline stage5 在每轮 consolidation 调用，扫描 `episodic_events` 工具调用事件计算技能成功率；成功率低于 `M6Skill.EvolutionSuccessThreshold`（0.6）且使用 ≥ `EvolutionMinUsage`（10）时，复用 `LogicCollapseCompiler` 触发重生成（vN→vN+1）。同一技能两次触发间隔 ≥ 1 小时（`evolutionCooldown`）；`running` 标志防多会话并发全表扫描。阈值见 `internal/config/thresholds.go` `M6Skill`。

**未实现（设计）**：SuccessHistory/FailureReasons 滑窗与 UpdateValidate（连续 3 次失败沙箱重跑测试，过则重置、否则 deprecated）/ UpdateReflect（LLM 分析失败生成改进版）/ UpdateDiscard（成功率 <30% 且使用 >10 移出主索引）三策略；UncontrollableFailure（网络不可达、API 配额耗尽、OS kill）不计入成功率的约定；连续 >100 次 UncontrollableFailure 冻结废弃评估并每 60s 探测恢复（连续 3 次成功解冻）。落地时须保留"外部故障不惩罚技能质量"原则。

### 4.2 四级废弃

- 级别: 普通更新 | 触发条件: LLM 生成更好版本 | 操作: version++, 旧版本保留 | 可逆: 可回退
- 级别: 验证过滤 | 触发条件: 连续 3 次测试失败 | 操作: deprecated=true, 仍可检索 | 可逆: 可手动解除
- 级别: 动态废弃 | 触发条件: 成功率 < 30% 且使用 > 10 次 | 操作: 移出主索引 → 废弃池 | 可逆: 需管理员恢复
- 级别: 硬删除 | 触发条件: 安全漏洞/签名失效 | 操作: 物理删除脚本文件 + 撤销签名 | 可逆: 不可逆

### 4.3 ContextHint — 运行时兼容性

Logic Collapse 把生成瞬间的 M5 Persona 与 M9 Activation Steering 隐式固化进脚本；用户切换偏好后 System 1 命中旧脚本会输出不一致。

唯一已落码的约束：绝对禁止表现层风格（语气/冗长度/格式化）硬编码到脚本，经 `context_hint` 运行时注入。

**[未实现] 运行时 Persona 指纹兼容性检查**（DR-4-002 裁决）：曾设计"生成期记录 Persona 指纹、System 1 命中时比对决定 Cache Miss"，全仓无任何实现且 ROADMAP 未登记，故删除其伪规格（避免误导按图索骥）。问题本身（偏好切换后 System 1 误命中固化旧偏好的脚本）真实存在且未解决；落地须先经 Eval Harness 量化误命中率，再新立 ADR。

---

## 5. 脚本缓存策略

沙箱执行配置见 [Script-Sandbox]（M7 权威源）；本节仅含 M6 脚本进程池缓存。

### 5.1 分层预加载

`ScriptSkillCache`（`internal/extension/skill/script_skill_cache.go`，`cmd/polaris/boot_agent.go` 注入 Agent FastPath）已实现；进程池由 M7 沙箱层注入。

- 等级: 金牌 | 条件: 成功率 > 90% 且使用 > 50 | 策略: 启动时预热进程池常驻 | 上限: M03 §5.3 `SkillPreloadGold`
- 等级: 银牌 | 条件: 成功率 > 70% 或 7 天使用 > 10 | 策略: 首次调用后常驻 | 上限: M03 §5.3 `SkillPreloadSilver`
- 等级: 铜牌 | 条件: 其余已入库 | 策略: 按需启动，30min TTL + LRU 驱逐 | 上限: M03 §5.3 `SkillPreloadBronze`

**GetOrSpawn**：goldCache → silverCache → bronzeCache（TTL+LRU touch）→ 按需启动 → promoteOrCache（成功率>0.9 && 使用>50 → gold；成功率>0.7 || 7天使用>10 → silver；其他 → bronze）。金牌启动时异步预热（goroutine pool），不阻塞 Agent 就绪；预热完成前可用 SKILL.md 解释执行 fallback。

**与 M5 的关系**：ScriptSkillCache 是 M5 ProceduralMemory.skillKV 的进程池缓存层（skillKV=持久化 SkillBlob，Cache=内存预热进程池）；淘汰只丢进程池，不影响持久化。

**崩溃恢复**：进程池无需持久化，崩溃后从 SurrealDB-Core KV 的脚本文件重启（同源码 → 同行为）。沙箱版本升级后 M12 Eval Harness 自动对全部 Gold 技能回归测试——全过则兼容性确认；失败者标 `needs_revalidation` + WARN，回退 SKILL.md 解释执行。

### 5.2 Deny by Default

默认允许：基本计算、只读时钟、安全随机源。默认禁止：文件系统、网络、进程创建、系统调用、原始内存访问。未声明能力的请求 → Rust 沙箱拒绝 → M4 降级。资源硬限制见 [Script-Sandbox]。

---

## 7. 降级与失败模式

- 故障场景: SkillSelector 未匹配任何技能 | 降级路径: 退到 LLM 通用工具调用路径 | 恢复策略: 新技能注册后自动生效
- 故障场景: 脚本生成失败 | 降级路径: `ErrLogicCollapseDisabled` + 降级 SKILL.md 模式 (WARN) | 恢复策略: LLM 蒸馏失败缓存标记，下次重试
- 故障场景: HMAC-SHA256 签名校验失败 | 降级路径: 拒绝加载（fail-closed）+ CRITICAL 审计 | 恢复策略: 重新签名或回滚旧版本
- 故障场景: 技能执行超时 | 降级路径: 硬 kill（超时见 `spec/state.yaml §m6_skill.skill_exec_timeout_low_seconds` / `skill_exec_timeout_medium_high_seconds`）+ ErrSkillTimeout | 恢复策略: 下次调用正常执行
- 故障场景: 技能缓存 LRU 驱逐 | 降级路径: 冷加载 (~100ms) | 恢复策略: 热度恢复后自动缓存

与 OSMemoryGuard 协同：M3 L2 紧急 → 暂停 Logic Collapse（禁止新脚本生成）。Tier 0 Bronze 并发进程数硬上限 1。

## 默认参数

阈值与重评触发条件: `spec/state.yaml §thresholds.m6_skill`。

## 8. 跨模块依赖与契约

- 关联模块: M4 Agent Kernel | 关键契约: System 1 缓存命中 → SkillExecutor.ExecuteSkill、Persona 兼容性检查（未实现，§4.3）、SkillSelector.SelectTopK | 位置: M4 §5, M6 §4.3
- 关联模块: M5 Memory | 关键契约: L3 Procedural Memory（SurrealDB-Core skillKV + 检索）、skillKV 与 ScriptSkillCache 的关系 | 位置: M5 §5, M6 §5.1
- 关联模块: M7 Tool Action | 关键契约: Rust Script Sandbox（CANONICAL SOURCE，wasmtime_engine.rs）、能力权限矩阵 | 位置: M7 §4
- 关联模块: M9 Self-Improve | 关键契约: Logic Collapse 触发 → M6 编译流水线 | 位置: M9 §1.1
- 关联模块: M11 Policy Safety | 关键契约: Skill RiskLevel 评估、HMAC-SHA256 签名验证、SandboxTier 分配 | 位置: M11 §2, M7 §5
- 关联模块: M12 Eval Harness | 关键契约: 技能编译前 Eval Gate 自动化回归 | 位置: M12 §7
- 关联模块: 全局字典 | 关键契约: Logic-Collapse/Script-Sandbox 定义、HE-Rule-3 可组合原语 | 位置: 00-Global-Dictionary §5, §2
- 关联模块: DDL | 关键契约: Skill 存储物理路径（SQLite skills 表，见 §2.2） | 位置: internal/protocol/schema/008_skills.sql

---

## 9. AgentSkills 标准格式适配

> 开放标准 agentskills.io（Anthropic 发布，Codex / Claude Code / Copilot / Cursor / Gemini CLI 共同采纳）。Polaris 完整实现，并与 Codex `.polaris-plugin/plugin.json` 的 `"skills"` 字段对齐。

### 9.1 SKILL.md 完整 frontmatter

字段：`name`（技能 slug，kebab-case）、`description`（一句话，用于 buildToolSchemas()/SkillSelector 匹配）、`version`、`tags`、`exec_mode`（`tool` 默认 | `ambient`）、`risk_level`（`low`|`medium`|`high`）、`sandbox`（`L1`|`L2`|`L3`）、`capability`（描述性能力标签）、`ambient_priority`（`always`|`auto` 默认|`index_only`，控制 ambient 注入优先级）；frontmatter 之后为正文（使用指令/决策树/规范）。

| exec_mode | 含义 | 系统处理 | 用户感知 |
|-----------|------|---------|---------|
| `tool`（默认） | 可调用命令，LLM 主动选择执行 | `buildToolSchemas()` 暴露为 `skill__{slug}` 工具 | 监控页"技能"列表，可 `/skill-name` 调用 |
| `ambient` | 背景知识，Agent 自动加载上下文 | `buildAmbientSkillsSection()` 注入 system prompt | 监控页"技能"列表（标注 ambient），无需主动调用 |

### 9.2 skill name 命名规范

| 来源 | 格式 | 示例 |
|------|------|------|
| 独立安装（marketplace/user） | `skill:{ext_id后缀_hex}` | `skill:abc12345def67890` |
| 插件子 Skill | `skill:{plugin-name}/{skill-slug}` | `skill:polaris-computer-use/computer_use` |
| 内置 Skill | `skill:builtin/{name}` | `skill:builtin/system_probe` |

所有路径统一经 `SkillRegistry.Register()`，强制校验 `skill:` 前缀（禁止裸 SQL 绕过）。

### 9.3 plugin_id 字段

`skills.plugin_id`（FK → `plugins.id`）标识归属：独立安装 `""`，插件子 Skill `"pl_{hex}"`。级联：插件禁用 → `UPDATE skills SET deprecated=1 WHERE plugin_id=?`；启用 → `deprecated=0`；卸载 → `DELETE FROM skills WHERE plugin_id=?`（硬删除）。

### 9.4 两条执行路径

| 路径 | runtime | exec_mode | 触发 | 执行 |
|------|---------|-----------|------|------|
| LLM tool_use | `script` | `tool` | buildToolSchemas() 暴露 | toolExec 读 instructions + input → LLM |
| System Prompt 注入 | `script` | `ambient` | 每次推理请求自动注入 | `internal/gateway/server/chat/system_prompt_ambient.go` 写入 system role |
| M6 SkillSelector | `script` | `tool` | M4 System 1 命中 | SkillExecutor → ContainerSandbox（Python runtime） |

**代码位置**：
- `internal/gateway/`：frontmatter 解析、工具 Schema 构建（exec_mode='tool'）、ambient 注入（exec_mode='ambient'）
- `internal/extension/skill/`：SkillRegistry（Register/List/Get）+ `compile.go` LogicCollapse 白盒分析蒸馏流水线（System 2 轨迹 → Python 代码生成 → ValidatePython → ContainerSandbox 行为测试 → DB 注册）；`skill_creator.go` GenerateSkill 为用户意图驱动 LLM 代码生成（intent → LLM → SKILL.md + plugin.json 写磁盘，独立于 LogicCollapse、不走 CFG 分析，写文件后经 `SkillRegistry.Register`（含 exec_mode）入库）

**双流水线区别**：`compile.go` = 白盒轨迹分析驱动（LogicCollapse，自动化）；`skill_creator.go` = 用户意图驱动 LLM 代码生成（手动触发）。

---
