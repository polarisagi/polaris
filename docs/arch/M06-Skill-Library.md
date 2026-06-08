# 模块 6: Skill Library

> 可命名、可参数化、可索引的复用技能。Go 主导管理+检索+Logic Collapse，Wasm 沙箱执行。[HE-Rule-3] [HE-Rule-5]
> **§跳读**: 0-bis:5 职责 / 0-ter:15 不变量速查 / 1:26 技能表征 / 2:62 生命周期(CANONICAL) / 3:219 检索系统 / 4:256 演化 / 5:293 Wasm缓存 / 7:327 340(SOFT)降级 / 8:341 依赖
## 0-bis. 职责边界

- M6 **是**: 技能注册、索引、检索、生命周期管理 | M6 **不是**: 技能沙箱执行（那是 M7）
- M6 **是**: Logic Collapse 蒸馏（System 2 轨迹 → Wasm 编译） | M6 **不是**: LLM 推理调用（那是 M1）
- M6 **是**: SkillSelector 启发式匹配（不调 LLM） | M6 **不是**: Agent 状态机控制（那是 M4）
- M6 **是**: cosign 签名验证（加载前置） | M6 **不是**: 签名策略制定（那是 M11 Cedar-Gate）
- M6 **是**: SKILL.md（script runtime）或 SKILL.md + impl.wasm（wasm runtime）技能管理 | M6 **不是**: 工具注册与发现（那是 M7 ToolRegistry）

---

## 0-ter. 不变量速查表

- 编号: inv_M6_01 | 不变量: SkillSelector 不调 LLM——启发式 + 向量匹配 + 排序公式，保持确定性 | 验证方式: 代码审计 + CI `skill_selector_llm_free`
- 编号: inv_M6_02 | 不变量: Logic Collapse 产物必经 staging 7 阶段——禁止 M9 直写 skills 表 | 验证方式: M9 → M2 Outbox 路径审计
- 编号: inv_M6_03 | 不变量: risk_level 缺失/歧义时默认最高风险——SandboxTier 取最严格级别 | 验证方式: M11 PolicyGate 代码审计
- 编号: inv_M6_04 | 不变量: 仅幂等技能可缓存结果——非幂等技能每次重新执行 | 验证方式: SkillCache key 包含 idempotency 标记
- 编号: inv_M6_05 | 不变量: cosign 签名校验失败 → fail-closed 拒绝加载 + CRITICAL 审计 | 验证方式: M6 §2 验证流水线 Step 4
- 编号: inv_M6_06 | 不变量: 编译前安全闸门全部满足才触发 Logic Collapse——成功次数/语义方差/Eval Gate | 验证方式: M6 §2.2 编译前安全闸门

---

## 1. 技能表征

### 1.1 目录结构

**script runtime**（SKILL.md 指令，LLM 执行）：
```
skill-name/
├── SKILL.md            # NL 描述 + 使用指令（全文存入 skills.instructions）
└── agents/polaris.yaml # 可选：display_name/policy/dependencies
```

**wasm runtime**（Logic Collapse 编译产物，wazero 执行）：
```
skill-name/
├── SKILL.md            # NL 描述
├── schema.json         # 入参/出参 JSON Schema
├── impl.wasm           # Wasm 编译产物 (wazero 执行)
├── impl.go             # Go 源码 (审计用)
├── test/               # 测试用例 (Eval Harness 输入)
└── SIGNATURE           # cosign 签名
```

script runtime 技能由市场安装（SKILL.md 即最终产物）；wasm runtime 技能由 Logic Collapse 从 script 轨迹蒸馏编译生成。

### 1.2 Go 数据结构

Skill/JSONSchema/Condition/SkillSource 类型定义见 `pkg/extensions/skill/skill.go`。

依赖环检测: Register 时对 DependsOn ∪ ComposesOf 构建有向图 → DFS 三色染色 (White/Gray/Black) 检测后向边。存在环 → `ErrCircularDependency{Path}`。O(V+E), V≤100, <1ms。ComposesOf 与 DependsOn 边在环检测中等价处理。

版本更新: 子技能 Version++ → SkillIndex 反向依赖扫描 → 标记父技能 `needs_compat_check` → 下次调用时半隔离 Wasm 沙箱执行集成测试 (最新 10 个用例)。通过 → 自动更新 pin 版本；失败 → 保持旧版本 + WARN + 通知 M8。

版本约束: `"skill-id@v1"` 锁定主版本 (允许 1.x 补丁), `"skill-id@v1.2"` 锁定次版本, `"skill-id@v1.2.3"` 锁定精确版本。Patch 递增 (v1.0→v1.1) 不触发父技能兼容检查；Minor/Major 递增触发。

---

## 2. 技能生命周期

### 2.1 五阶段流水线

技能从成功轨迹到可执行 Wasm 经过五个阶段:

**Stage 1 — 轨迹记录**: Agent 成功完成多步任务后，完整执行轨迹（含 LLM 调用、工具调用、环境上下文）持久化到 Episodic Memory 的 EventLog。

**Stage 2 — 非确定性剥离**: 将完整轨迹中的概率性部分（LLM 推理链）与确定性部分（工具调用）分离。LLM 调用中的具体决策提取为硬编码确定性参数，丢弃 LLM 中间推理链。环境上下文（绝对路径 → `{workspace}` 占位符、时间戳 → 相对时间、主机名/用户名/IP 移除）做平台无关化。仅保留确定性工具调用序列作为输出。

**Stage 3 — 参数化抽象**: 识别 Stage 2 输出中的可变参数（路径 → `{input_path}`、搜索词 → `{search_pattern}`、数值 → `{threshold}`），自动推断类型并生成 InputSchema 和 OutputSchema。提取默认值，标记 required 字段。

**Stage 4 — 契约推导**: 默认采用 DeepSeek V4 推导技能的前置条件（所需环境/工具/权限）、后置条件（验证标准）、风险等级和沙箱 tier。

**Stage 5 — Wasm 编译 + 索引 + 签名**: impl.go 经 AST 脱敏后发送至远程 TinyGo 编译服务 → 双源编译验证（两个独立服务产出相同 WasmHash）→ 本地 wazero 验证 → cosign 签名 → 写入 Skill Index → SurrealDB-Core KV 缓存。

### 2.2 Logic Collapse: System 2 → System 1

> **实现状态**：`FeatureLogicCollapse` 特性门控已定义（Tier1+，≥1GB free 自动启用）。编译流水线已实现于 `pkg/extensions/skill/compile.go`（`LogicCollapseCompiler`）：FreshnessChecker → DataStripper → CompileGate → LLMCodeGenerator → StaticCFGAnalyzer → TaintSanitizeForRemoteCompilation → DualSourceCompiler（双源 WasmHash 验证）→ wazero 魔数验证 → 风险分级 → HMAC-SHA256 签名 → SkillRegistry 写入。M9 触发器实现于 `pkg/swarm/logic_collapse_trigger.go`（`LogicCollapseMonitor`，Welford 在线语义方差估算 + HITL 分流）。Tier 0 仍仅加载预编译技能（CompileGate 拦截）。

```
System 2 成功执行 → 轨迹分析 (识别确定/概率部分) → LLM 代码生成 (Go) → TinyGo → Wasm → wazero 验证 → 签名入库
→ System 1: 同类任务 SurpriseIndex < 0.2 → Wasm 技能直接命中，零 LLM 推理 [SurpriseIndex]
```

**编译策略**: 技能先以 SKILL.md 形态存在，累积 >= 50 次成功 + 语义方差检查 + HITL/Eval Gate → Logic Collapse → Wasm 编译

**双源编译验证**（供应链攻击防御）: 同时发送给两个独立编译服务，比对 WasmHash 一致才入库。编译产物本地 wazero 重新解析做静态分析（CFG/syscall 审计）作为入库前最后一关。

**local_only 编译替代方案**: 远程编译服务不可用时：
- Tier 1+ → 本地 TinyGo 编译（需要 ~500MB TinyGo 工具链 + LLVM）
- Tier 0 → Logic Collapse 禁用，仅加载预编译技能
启动时由 `FeatureGate.FeatureLogicCollapse` 自动判定（≥Tier1且≥1GB free→启用，否则仅预编译）。调用方无需手写 if-else。详见 M03 §5。
- 用户首次进入 local_only 模式时主动提示能力降级影响

**编译前安全闸门** (全部满足才触发 Logic Collapse):
1. 成功次数 >= 50
2. SemanticVarianceCheck: 50 次成功轨迹输入 embedding 方差 < 0.1 → 拒绝，标记 `needs_more_diversity`
3. HITL/Eval Gate:
   - RiskLevel >= "high" → HITL Gateway [ESCALATE]
   - RiskLevel < "high" → M12 Eval Harness 自动化回归测试
   - Day-0 冷启动分级阈值 (`minEvalCasesPerSkill=5`):
     (a) 黄金用例=0 且成功≥50 → Auto-Eval-Bootstrapping: M12 放开抽样池，采用 DeepSeek V4 批量审查（抽取大量最分散轨迹，如 20 条），进行深度交叉审查 (越权/数据泄漏/行为偏差)。全部通过 → `source=synthetic_auto_bootstrap`；任一未通过 → `needs_review`
     (b) 0<用例<5 → 降低阈值至实际用例数，通过 → `eval_coverage_partial`
     (c) 用例=0 且成功<50 → `ErrInsufficientEvalCoverage`

**编译前静态分析** (LLM 生成 impl.go 后、发送远程编译前):
1. 控制流图 (CFG) 分析: 检测不可达代码分支、条件性休眠/定时激活模式（时间炸弹特征）
2. 系统调用审计: 扫描所有 `//go:wasmimport` 和 host function 调用，与 WASI 权限矩阵交叉验证——未声明能力的调用 → 拒绝
3. 确定性审查: 代码中不得包含 `time.Now()`、`rand.Read()`、`os.Getenv()` 等非确定性调用——这些必须通过 `context_hint` 运行时注入
4. 上述分析失败 → 拒绝编译，轨迹进入 MEMF + 写 `skill_static_analysis_rejected` 审计事件

**编译方案**:
- 远程编译服务: 发送脱敏后 impl.go → 容器化 TinyGo → 返回 Wasm + 编译日志 (stdout/stderr hash)
- Reproducible Build 要求: 同一 impl.go + 同一 TinyGo 版本 → 确定性 Wasm 字节码 (SHA-256 可复现)。编译日志 hash 用于交叉验证——两个独立编译实例产生相同 WasmHash → 供应链接管风险可控
- local_only 模式: Logic Collapse 禁用，仅加载预编译技能。触发时若 `privacyMode=="local_only"` → `ErrLogicCollapseUnavailableInLocalOnly`；降级到 SKILL.md 模式 (WARN)
- Tier 0 预编译技能库随版本发布，覆盖 System 1 核心能力面

**并发控制**:

- Tier: Tier 0 (8GB) | 并发编译数: 1 (串行)
- Tier: Tier 1 (16GB) | 并发编译数: 2
- Tier: Tier 2+ (24GB+) | 并发编译数: 4

并发限制同时约束 Logic Collapse 编译路径 + SkillIndex lazy JIT 编译路径 (Silver/Bronze)。JIT 编译阻塞等待 ≤5s，超时 fallthrough → SKILL.md LLM 执行。

CompileGate 准入: 空闲内存 >= 80MB (50MB 技能预算 + 30MB 安全边距) [Tier-0-Limit]

**编译主流程**: ① privacyMode=="local_only" → 降级 SKILL.md ② compileGate() 内存检查 ③ canStartNewCompile() 并发检查 → 通过后执行编译

**WASI 虚拟化路径映射** (跨平台可移植性):
- `/workspace/` → M2 Workspace 当前 task 目录
- `/tmp/sandbox/` → `os.TempDir()/polaris_sandbox_{skill_id}/`
- `/usr/local/bin/` → 白名单工具目录
- 宿主绝对路径 → 沙箱内不可见；技能通过 M7 Workspace Bridge host function 按需获取文件内容
- 一次编译，三平台运行

**远程编译数据安全** (AST 脱敏 — `TaintSanitizeForRemoteCompilation`):
脱敏实现见 `pkg/extensions/skill/compile.go`（`TaintSanitizeForRemoteCompilation`）：解析 impl.go AST，将字符串/数值/标识符/注释替换为参数化占位符，保留包路径与协议关键字，生成 `redaction_map.json` 存本地，确保 PII 不离开本机。编译后 Wasm 通过线性内存传递参数:

**Wasm ABI (共享线性内存方案)**:
- Wasm 导出函数: `run(ptr i32, len i32) -> (result_ptr i32, result_len i32)`。wazero/Wasm 核心规范仅支持 i32/i64/f32/f64 基本数值类型，Go 切片类型不可直接跨 FFI 传递
- 调用前: Go 宿主侧在 Wasm 线性内存中申请 buffer (`module.Memory().Write`)，写入 JSON 序列化的参数 `{"strParams":[...], "intParams":[...], "floatParams":[...]}`
- 调用: 传递内存偏移量 `ptr` 和字节长度 `len` 给 Wasm `run` 函数
- Wasm 内: TinyGo 代码通过 `ptr` + `len` 从线性内存反序列化 JSON → 执行技能逻辑 → 将结果为 JSON 写回线性内存 → 返回 `(result_ptr, result_len)`
- 宿主侧: 从线性内存 `result_ptr` 读取 `result_len` 字节 → JSON 反序列化 → ToolResult
- 参数值注入: `redaction_map.json` 中的原始参数值在宿主侧 JSON 序列化时还原（Wasm 不可访问原始敏感值，仅接收脱敏后参数）

每个技能仅接收自身声明的参数值 (最小权限)，禁止全局 PII 访问接口。

**Trace Data Stripping** (LLM 代码生成前 — 轨迹数据最小化):
LLM 仅接收: (a) 工具调用序列类型签名 (InputSchema+OutputSchema，不含参数值), (b) 成功/失败状态, (c) 执行顺序 DAG 拓扑。参数值仅保留类型信息 + 长度/大小元数据。不可逆 strip-only (数据丢弃非脱敏)。Data Stripping 在 AST 脱敏之前执行——保护 LLM 代码生成路径。

**Freshness Check** (源轨迹时效性验证):
Logic Collapse 触发前验证源轨迹关键决策是否被后续 Semantic Memory 更新 supersede。
约束: 500ms 超时，O(N*M), N=toolCalls, M=entities per call。
步骤: 遍历工具调用 → 检查实体/关系 UpdatedAt > trace.CompletedAt → 标记 StaleEntity/StaleRelation。不 Fresh → markTraceAsStale；Fresh → 返回 FreshnessResult{Fresh: true}。
失败不阻塞系统 → 轨迹标记 `needs_adaptation` 等待 M9 下一轮评估。

**Logic Collapse 调用顺序**: `freshnessCheck → dataStripping → compileGate → canStartNewCompile → LLM 代码生成 → AST 脱敏 → 远程编译`

**命名空间规则与重名冲突处理**:

M6 将技能命名空间与 Built-in 工具命名空间物理隔离：

- 命名空间前缀: `skill:` | 归属: SkillLibrary 管理的技能（Built-in + Logic Collapse 生成 + 用户自定义） | 示例: `skill:file_read`
- 命名空间前缀: `tool:` | 归属: M7 ToolRegistry 注册的 Built-in 原语工具 | 示例: `tool:file_read`

SkillLibrary.Register 强制要求技能 ID 以 `skill:` 为前缀。M7 ToolRegistry.Register 强制要求工具名以 `tool:` 为前缀（或无前缀，向后兼容）。两者路由由 M4 RouteReasoning 在 System 1 命中阶段分别查找，**不存在跨命名空间同名覆盖**。

并发 Logic Collapse 产出同名技能冲突规则：

M9 BackgroundTaskScheduler 允许多个 Logic Collapse 任务并发排队（L1 优先级）。若两个 Logic Collapse Worker 同时为不同语义聚类生成 `skill:X` 时：

1. SkillLibrary.Register 在写入 skills 表前执行 `SELECT skill_id, semantic_cluster_id FROM skills WHERE skill_id = ?`。
2. 若已有同名技能且 `semantic_cluster_id` 不同（语义不同源冲突）：以 `candidate_emit` 时间戳**较晚者**的产物为准（后入覆盖前入），写 `skill_name_collision` 审计事件（含两次 emit 的 semantic_cluster_id、聚类中心距离）。
3. 若 `semantic_cluster_id` 相同（同语义重复提交）：按 `version++` 正常更新，不视为冲突。
4. 覆盖写完成后，M4 DAGNode 中已有对旧版本 `skill:X` 的技能引用不受影响（引用锁定 `skill:X@v{N}`，新版本为 `skill:X@v{N+1}`），遵循 §1.2 版本约束规则。

**Skill 存储物理路径**: 遵循 M2 Outbox 模式。SQLite 单事务原子写入 `skills` 表 + `events` 表 (`SkillCreatedEvent`) + `outbox` 表 (`target_engine='SurrealDB-Core'`)。Outbox Worker 异步投影 Wasm blob 到 SurrealDB-Core KV [Storage-SurrealDB-Core]。SKILL.md + test/ 为文件系统 Ground Truth (Git 版本控制) [Storage-SQLite]。

**技能创建后演化**: Logic Collapse 仅在创建时做一次轨迹→Wasm 蒸馏。Hermes 双环模式表明,技能可通过持续使用轨迹(成功率、边缘案例分布)自动触发再编译——不依赖初始聚类,而是基于在线反馈(成功率、用户纠错频率)驱动 vN→vN+1 迭代。预留 `skill_evolution_trigger` 接口: 当技能近 N 次使用成功率低于阈值或出现新语义类别轨迹时, M9 BackgroundTaskScheduler 将其排入再编译队列。当前 phase 不做,仅标注设计空间。

**OpenClaw 技能迁移**: `polaris migrate openclaw --preset=skills` 可将 OpenClaw 的 SKILL.md 脚本拷贝到 polaris workspace/skills/ 目录。但 OpenClaw 技能是纯脚本执行(无沙箱编译),polaris 需要 Wasm 沙箱——迁移后的 SKILL.md 无法自动转换,需人工执行 Logic Collapse 编译为 Wasm。详见 M13 §1.1 "外部平台迁移"。

### 2.3 技能验证流水线

```
LLM 生成的 impl.go
  │
  ├→ Step 0: Taint-Check [Taint-Medium] [Taint-Floor-Medium]
  │   放行: TaintLow (用户输入) 或 TaintNone (系统编译期) → 编译
  │   拒绝: TaintMedium+ 轨迹严禁编译 → 进入 MEMF, 标记 tainted_trajectory
  │   原则: 污点永不静默消除。编译产物保持输入 TaintLevel 感知并传播到输出 [Taint-Prop]
  │
  ├→ Step 1: 静态分析 (AST syscall 审计)
  │   禁止 import "os/exec", "net/http" (RiskLevel=high 除外)
  │   禁止 unsafe 包、CGO
  │   函数签名必须匹配 schema.json
  │
  ├→ Step 2: wazero 沙箱行为测试
  │   for each test case in test/:
  │     创建 wazero 沙箱 → 注入受控 Host Functions (只读文件) → 运行 impl.wasm → 对比输出
  │   10,000 随机输入模糊测试 (fuzz)
  │   全部通过 → Step 3；失败 → 打回 LLM 修复 (最多 3 轮)
  │
  ├→ Step 3: 风险分级
  │   文件写入请求 → RiskLevel=medium；网络请求 → RiskLevel=high；无外部请求 → RiskLevel=low
  │   分配 SandboxTier [Sandbox-L2]
  │
  └→ Step 4: 签名 + 入库
      cosign sign → SIGNATURE 文件 → 写入 Skill Library
      签名私钥不对远程编译器暴露
```

---

## 3. 技能检索系统

### 3.1 三级检索

```
SkillRetriever:
  L1 vecIndex: embedding → top-N 语义检索, SemanticK=10 [Storage-SurrealDB-Core]
  L2 sigMatcher: 任务特征哈希 → 行为相似技能, SignatureK=5
  L3 depGraph: PPR 遍历技能依赖图, GraphDepth=2, PPRAlpha=0.6

RetrievalConfig: SemanticWeight=0.5, SignatureWeight=0.3, GraphWeight=0.2, ContextBudget=2000 token

Retrieve 流程:
  Stage 1 — 并行三路检索: errgroup (a) vecIndex.Search (b) sigMatcher.Match (c) depGraph.PPRTraverse
  L1 降级链 (embedding 维度变化 → ErrDimensionMismatch):
    1. FTS5/BM25 文本检索: GoalDescription + InputSchema JSON + 工具名集合 → FTS5 MATCH [Storage-SQLite]
    2. 签名权重提升: SemanticWeight→0, SignatureWeight→0.7, GraphWeight→0.2, StructHashWeight→0.1
    3. Lazy Re-embedding: 标记 stale → M9 后台重嵌 → atomically swap vecIndex
  Stage 2 — 加权融合
  Stage 3 — 上下文预算 hydration: 渐进披露 (name+desc → workflow summary → full instructions), ContextBudget 内截断
```

### 3.2 结构签名匹配

技能检索使用两级签名进行精确匹配:

- **IntentSignature（路由预检级）**: M4 在 S_PLAN 前使用——基于目标描述哈希、输入类型、输出类型和领域提示，快速判断是否命中 System 1 缓存。匹配度阈值 ≥ 0.8。
- **ExecutionSignature（缓存替换级）**: DAG 编译后使用——工具调用序列哈希 + DAG 拓扑哈希（节点数 + 边集 + 并行度），精确去重。匹配度阈值 ≥ 0.95。

两级签名配合: IntentSignature 做粗筛（快但精度较低），ExecutionSignature 做精筛（慢但精度高）。最终按成功率排序返回 topK 技能。

### 3.3 PPR 依赖遍历

技能的依赖图（DependsOn + ComposesOf）通过 Personalized PageRank 算法进行遍历检索。DependsOn 边支持双向遍历（依赖和被依赖都需要检索），ComposesOf 边仅向上（子→父）遍历——不反向展开以避免检索膨胀。BFS 收集候选后，PPR 以 alpha=0.6（60% 随机游走、40% 跳回种子）计算节点分数，按分数降序排序。

---

## 4. 技能演化

### 4.1 递归演化

技能根据历史执行记录自动演化。每个技能维护最近 N 次执行结果的 SuccessHistory 和 FailureReasons，以及三种更新策略:

- **UpdateValidate（验证型）**: 连续 3 次失败时重新在沙箱中运行现有技能的测试用例——通过则重置失败历史，不通过则标记 deprecated
- **UpdateReflect（反思型）**: 连续失败时由 LLM 分析失败原因并生成改进版本的 impl.go，版本号递增
- **UpdateDiscard（废弃型）**: 成功率低于 30% 且累计使用超过 10 次时移出主索引

UncontrollableFailure（网络不可达、API 配额耗尽、OS kill）不计入 SuccessHistory——不因为外部故障惩罚技能质量。SuccessHistory 保留最近 20 条记录。连续 UncontrollableFailure 超过 100 次时冻结废弃评估，改为每 60 秒探测一次恢复状态；连续 3 次成功 → 解冻。

### 4.2 四级废弃

- 级别: 普通更新 | 触发条件: LLM 生成更好版本 | 操作: version++, 旧版本保留 | 可逆: 可回退
- 级别: 验证过滤 | 触发条件: 连续 3 次测试失败 | 操作: deprecated=true, 仍可检索 | 可逆: 可手动解除
- 级别: 动态废弃 | 触发条件: 成功率 < 30% 且使用 > 10 次 | 操作: 移出主索引 → 废弃池 | 可逆: 需管理员恢复
- 级别: 硬删除 | 触发条件: 安全漏洞/签名失效 | 操作: 物理删除 Wasm + 撤销签名 | 可逆: 不可逆

### 4.3 ContextHint — 运行时兼容性

Logic Collapse 将编译瞬间的 M5 Persona 和 M9 Activation Steering 隐式固化为代码行为。用户切换偏好时 System 1 命中旧 Wasm → 输出不一致。

结构约束:
1. 绝对禁止表现层风格 (语气/冗长度/格式化) 硬编码到 Wasm — 通过 `context_hint` 运行时注入
2. 每个 Wasm 编译时记录 `CompiledPersonaFingerprint{InteractionSummaryHash, ActiveCVLabels, VerbosityPref, ResponseFormat, CompiledAt}`
3. M4 System 1 命中后对比当前 Persona 指纹 vs 编译指纹 — 关键维度变更 → Cache Miss → System 1.5 LLM 接管

```
IsPersonaCompatible:
  步骤1: CompiledPersonaFingerprint == nil → 始终兼容 (内置/用户定义)
  步骤2: VerbosityPref 或 ResponseFormat 不一致 → 不兼容
  步骤3: subtractive cv label 变更 (编译时 label 被移除) → 不兼容；additive → 兼容
```

---

## 5. Wasm 缓存策略

wazero 运行时配置见 [Wasm-Sandbox] (M7 权威源)。本节仅含 M6 编译产物缓存策略，不重复沙箱执行细节。

### 5.1 分层预加载

- 等级: 金牌 | 条件: 成功率 > 90% 且使用 > 50 | 策略: 启动时预编译常驻 | 各 Tier 上限见 M03 §5.3 TierParameterTable `SkillPreloadGold`
- 等级: 银牌 | 条件: 成功率 > 70% 或 7 天使用 > 10 | 策略: 首次调用编译后常驻 | 各 Tier 上限见 M03 §5.3 TierParameterTable `SkillPreloadSilver`
- 等级: 铜牌 | 条件: 其余已入库 | 策略: 按需编译，30min TTL | 各 Tier 上限见 M03 §5.3 TierParameterTable `SkillPreloadBronze`（Tier 0 ~50MB）

```
WasmSkillCache:
  L1 goldCache: map skill_id → *wazero.CompiledModule, 预编译常驻
  L2 silverCache: map skill_id → *wazero.CompiledModule, lazy 编译常驻
  L3 bronzeCache: map skill_id → *bronzeEntry{TTL=30min}, LRU 驱逐

GetOrCompile: goldCache → silverCache → bronzeCache (TTL+LRU touch) → 编译 → promoteOrCache
promoteOrCache: 成功率>0.9 && 使用>50 → gold; 成功率>0.7 || 7天使用>10 → silver; 其他 → bronze
```

金牌技能启动时异步预编译 (goroutine pool)，不阻塞 Agent 就绪。System 1 预编译完成前可用 SKILL.md 解释执行 fallback。

Tier 0 Bronze 并发编译硬限制 1: Bronze 按需编译瞬间额外占用 ~5-10MB（编译器临时数据），5 个 Bronze 同时编译 → 25-50MB 临时占用，叠加常驻 50MB → 总 75-100MB 超出 Wasm 预算。串行编译保障总 Wasm 内存 ≤50MB（含 5MB 编译临时缓冲区，实际常驻 ≤45MB）。M3 暴露 `polaris_wasm_memory_estimate_mb` Gauge，超 80MB → CRITICAL 卸载 Bronze。

**WasmSkillCache 与 M5 ProceduralMemory 的关系**: WasmSkillCache 是 M5 ProceduralMemory.skillKV 的内存编译缓存层。M5 skillKV = 持久化 SkillBlob（含 Wasm 字节码）；M6 WasmSkillCache = 内存中已编译的 wazero.CompiledModule（懒加载/预加载）。缓存淘汰时只丢弃编译产物，不影响 M5 持久化。

**崩溃恢复确定性**: wazero 不提供持久化编译缓存格式——CompiledModule 不可直接序列化落盘。崩溃后从 SurrealDB-Core KV 中缓存的 Wasm 字节码（.wasm blob）重新编译重建 CompiledModule。Wasm 字节码为确定性产物（同一 TinyGo 版本 + 同一源码 → 同一 WasmHash，由双源编译验证保证），wazero 编译行为遵循 Wasm 规范，不同 wazero 版本间的执行语义差异 ≤ 规范允许的 nondeterministic 指令（`memory.grow` 失败边界等）。wazero 版本升级后，M12 Eval Harness 自动对全部 Gold 级技能重新执行回归测试——全部通过则版本兼容性确认；失败技能标记 `needs_revalidation` + WARN，回退 SKILL.md 解释执行。

### 5.2 Deny by Default

默认允许: 内存分配、基本计算、只读时钟、安全随机源。默认禁止: 文件系统、网络、进程创建、系统调用、原始内存访问。技能请求未注入 Host Function → wazero 返回错误 → M4 降级。资源硬限制见 [Wasm-Sandbox]。

---

## 7. 降级与失败模式

- 故障场景: SkillSelector 未匹配任何技能 | 降级路径: 退到 LLM 通用工具调用路径 | 恢复策略: 新技能注册后自动生效
- 故障场景: Wasm 编译失败 | 降级路径: `ErrLogicCollapseUnavailableInLocalOnly` + 降级到 SKILL.md 模式 (WARN) | 恢复策略: LLM 蒸馏失败缓存标记，下次重试
- 故障场景: cosign 签名校验失败 | 降级路径: 拒绝加载（fail-closed）+ CRITICAL 审计 | 恢复策略: 重新签名或回滚旧版本
- 故障场景: 技能执行超时 | 降级路径: 硬 kill（超时见 `spec/state.yaml §m6_skill.skill_exec_timeout_low_seconds` / `skill_exec_timeout_medium_high_seconds`）+ ErrSkillTimeout | 恢复策略: 下次调用正常执行
- 故障场景: 技能缓存 LRU 驱逐 | 降级路径: 冷加载 (~100ms 延迟) | 恢复策略: 热度恢复后自动缓存

与 OSMemoryGuard 协同: M3 L2 紧急 → 暂停 Logic Collapse 编译（禁止新 Wasm 编译）。Tier 0 Bronze 并发编译硬上限 1。

## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m6_skill`。

## 8. 跨模块依赖与契约

- 关联模块: M4 Agent Kernel | 关键契约: System 1 缓存命中 → SkillExecutor.ExecuteSkill、Persona 兼容性检查、SkillSelector.SelectTopK | 位置: M4 §5, M6 §4.3
- 关联模块: M5 Memory | 关键契约: L3 Procedural Memory（SurrealDB-Core skillKV + SurrealDB-Core 检索）、M5 skillKV 与 M6 WasmSkillCache 的关系 | 位置: M5 §5, M6 §5.1
- 关联模块: M7 Tool Action | 关键契约: wazero Wasm sandbox（CANONICAL SOURCE）、WASI 权限矩阵 | 位置: M7 §4
- 关联模块: M9 Self-Improve | 关键契约: Logic Collapse 触发 → M6 编译流水线 | 位置: M9 §1.1
- 关联模块: M11 Policy Safety | 关键契约: Skill RiskLevel 评估、cosign 签名验证、SandboxTier 分配 | 位置: M11 §2, M7 §5
- 关联模块: M12 Eval Harness | 关键契约: 技能编译前 Eval Gate 自动化回归测试 | 位置: M12 §7
- 关联模块: 全局字典 | 关键契约: Logic-Collapse/Wasm-Sandbox 定义、HE-Rule-3 可组合原语 | 位置: 00-Global-Dictionary §5, §2
- 关联模块: DDL | 关键契约: Skill 存储物理路径（SQLite skills 表 + SurrealDB-Core Outbox 投影） | 位置: internal/protocol/schema/

---

## 9. AgentSkills 标准格式适配

> **开放标准**：agentskills.io（由 Anthropic 发布，OpenAI Codex / Claude Code / GitHub Copilot / Cursor / Gemini CLI 等共同采纳）。Polaris 完整实现此标准，并与 Codex `.polaris-plugin/plugin.json` 的 `"skills"` 字段规范对齐。

### 9.1 SKILL.md 完整 frontmatter

```yaml
---
name: computer_use              # 技能 slug（kebab-case）
description: "..."              # 一句话描述，用于 buildToolSchemas()/SkillSelector 匹配
version: "1.0.0"
tags: [computer-use, automation]
exec_mode: ambient              # "tool"（默认）| "ambient"
risk_level: medium              # "low" | "medium" | "high"
sandbox: L2                     # "L1" | "L2" | "L3"
capability: read-write          # 描述性能力标签
---

正文：使用指令 / 决策树 / 规范...
```

**exec_mode 语义**（核心区分）：

| exec_mode | 含义 | 系统处理 | 用户感知 |
|-----------|------|---------|---------|
| `tool`（默认）| 可调用命令，LLM 主动选择执行 | `buildToolSchemas()` 暴露为 `skill__{slug}` 工具 | 监控页面"技能"列表，可 `/skill-name` 调用 |
| `ambient` | 背景知识，Agent 自动加载上下文 | `buildAmbientSkillsSection()` 注入 system prompt | 监控页面"技能"列表（标注 ambient），无需主动调用 |

### 9.2 skill name 命名规范

| 来源 | 格式 | 示例 |
|------|------|------|
| 独立安装（marketplace/user） | `skill:{ext_id后缀_hex}` | `skill:abc12345def67890` |
| 插件子 Skill | `skill:{plugin-name}/{skill-slug}` | `skill:polaris-computer-use/computer_use` |
| 内置 Skill（builtin） | `skill:builtin/{name}` | `skill:builtin/shell_exec` |

所有路径统一经 `SkillRegistry.Register()`，强制校验 `skill:` 前缀（禁止裸 SQL 绕过）。

### 9.3 plugin_id 字段（2026-06 新增）

`skills.plugin_id`（FK → `plugins.id`）标识插件归属：
- 独立安装：`plugin_id = ""`
- 插件子 Skill：`plugin_id = "pl_{hex}"`

级联行为：
- 插件禁用 → `UPDATE skills SET deprecated=1 WHERE plugin_id=?`
- 插件启用 → `UPDATE skills SET deprecated=0 WHERE plugin_id=?`
- 插件卸载 → `DELETE FROM skills WHERE plugin_id=?`（硬删除）

### 9.4 两条执行路径

| 路径 | runtime | exec_mode | 触发 | 执行 |
|------|---------|-----------|------|------|
| LLM tool_use | `script` | `tool` | buildToolSchemas() 暴露 | toolExec 读 instructions + input → LLM |
| System Prompt 注入 | `script` | `ambient` | 每次推理请求自动注入 | injectSystemPrompt 写入 system role |
| M6 SkillSelector | `wasm` | `tool` | M4 System 1 命中 | SkillExecutor → wazero 沙箱 |

**代码位置**：
- `pkg/gateway/server/plugin_sync.go`：`parseFrontmatter()` / `parseSkillMD()`
- `pkg/cognition/skill/sqlite_registry.go`：`SkillRegistry.Register/List/Get`
- `pkg/gateway/server/server.go`：`buildToolSchemas()`（`exec_mode='tool'`）
- `pkg/gateway/server/sse.go`：`buildAmbientSkillsSection()`（`exec_mode='ambient'`）

---

