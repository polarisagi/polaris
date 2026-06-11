# docs/specs/ 变更日志

> 规范本身的演进记录。AI 每次会话开头扫描最近 5 条以感知规范增量。

格式：`YYYY-MM-DD | 文件 | 变更摘要`

## 2026-06-11（架构升级：技能执行迁移至 TypeScript 脚本 + Rust 沙箱）

- `M06/M09/M13-bis` | 技能执行从 TinyGo/impl.wasm/wazero 迁移至 TypeScript/Python 脚本（npx tsx），沙箱从 Go wazero 迁移至 Rust wasmtime（FFI）；内置工具直接信任不走沙箱；官方技能/插件移至独立仓库 polaris-plugins-official

## 2026-06-11（docs/arch/decisions + AGENTS.md + 02-Rust-FFI 全量修订）

**ADR 过时内容修正（4 个 ADR）**：

- `ADR-0005 §决策` | surreal_store.go cgo 状态描述由"历史遗留 P3 待处置"改为"cgo→purego 迁移已由 ADR-0011（2026-05-16）执行完毕"
- `ADR-0010 §关联ADR` | 补入 ADR-0011 引用，删除"cgo 偏离待 P3 处置"过时注释
- `ADR-0014 §决策` | 模型版本 "Opus 4.7"（不存在）→ "`claude-opus-4-8`"（Anthropic 当前最新旗舰）
- `ADR-0015 §状态/§2.1/§2.3/§4/§5` | 标注 §2.1 Plugin 层定位已被 ADR-0016 取代；SignatureValid 方案标注已被 TrustTier 替代；§4 "Plugin 放 M13"条目标注已被 ADR-0016 推翻；§5 补 ADR-0016 引用

**AGENTS.md（= CLAUDE.md）更新（6 处）**：

- Header `6 pkg` → `8 pkg`；ADR 清单补 `0020 DeepSeek V4 · 0021 核心机制实现`；DDL 清单末尾补 `029_workflows`，计数 25→26 张表

**docs/specs/ 规范修订**：

- `02-Rust-FFI.md RUST-1` | 删除过时"单文件结构可维持"描述（已拆分为 4 文件）
- `02-Rust-FFI.md RUST-3` | 更新文件组织为实际结构：`lib.rs`+`surreal_store.rs`+`wasmtime_engine.rs`+`check_wasi.rs`（旧描述为未落地的 cedar.rs/vector.rs 拆分方案）
- `02-Rust-FFI.md RUST-4` | 依赖白名单补充 `wasmtime`+`wasmtime-wasi`+`tokio`+`serde`+`serde_json`+`anyhow`+`bytes`+`lazy_static`（已在 Cargo.toml 实际使用，旧白名单漏列）

## 2026-06-09（Gemini gap 报告核查：修复两处真实差异）

**gap 核查结论（10 条 Gemini 报告）**：

- ID 4 CoEvolutionSubscriber / ID 5 PRM 无文档：假 gap；CoEvSubscriber 存在于 `pkg/swarm/self_improve_calibrator.go`；PRM 有完整文档 M04 §4.6
- ID 9 SemanticCache：真实gap — M01 §6.2 描述"无 Get/Put 实现"与代码实际（已实现）不符，已修复
- ID 8 onboard.html：真实gap — M13 §8.1 目录遗漏 `onboard.html`（14.8K），已修复
- ID 3 OnlineReindexer / ID 6 inv_ 测试：开发阶段已知缺口，暂保持文档意图，待代码补齐
- ID 1/2/7/10：部分真实，属"设计超前于实现"正常状态，不改动文档

commit 28c0915

## 2026-06-09（docs/specs 全量审查 + 架构文档一致性修复）

**docs/specs/ 补充修订**：

- `03-Agent-Pattern.md AGENT-1` | FSM 状态数 "10 态" → "11 态"（M04 §1 已加 S_INTERRUPT 为第 11 态）；流程图补 S_INTERRUPT 触发说明；System 1.5 上界 ≤0.6 → <0.6（与 `[System-1.5]` canonical 一致）
- `03-Agent-Pattern.md AGENT-5` | SurpriseIndex 公式替换为 canonical 三组件引用（旧公式含 `tokenBurnRate/maxBurnRate`+`actionSequenceEntropy`，与 `00-Global-Dictionary §3` canonical 不符）

**架构文档一致性修复（本会话 commit 1dfa428）**：

- `M04-Agent-Kernel.md §13` | Schema 引用 `003_tasks` → `007_tasks`
- `M09-Self-Improvement-Engine.md` | EvalGenerator `## 3.` → `## 3-bis.`（消除与主 §3 的重号）；同步更新 §跳读
- `M02-Storage-Fabric.md §16` | DDL 总量 21 份 → 25 份，范围扩至 028_apps
- `AGENTS.md` | DDL 清单补入 022~024/028，计数 20→25 张表

## 2026-06-09（架构文档全量审查 + specs 规范修订）

**架构文档（docs/arch/）深度审查修复（3 次提交，共 7 处）**：

- `M01-Inference-Runtime.md §4` | §4.4 编号重复（ComplexityDeterminer 与 Route 方法同编号），修正第二个为 §4.5，同步更新跳读索引
- `M02-Storage-Fabric.md §16` | DDL 覆盖描述"全部 6 份 DDL"严重低估，实为 21 份（001~021），改为正确引用 `internal/protocol/schema/`
- `M03-Observability.md §5.3` | TierParameterTable 中 `GraphRAGLLMDailyBudget` 参数与 M10 inv_M10_05"已取消财务日预算限制"直接冲突，改为 `GraphRAGConcurrentWorkers`（资源维度并发数）
- `M04~M13`（上轮）| 共修复 22 处：Gate/Stage 流水线映射统一、预算财务与资源门控逻辑冲突消除、跨模块契约不一致等

**ADR 体系修复**：

- `decisions/README.md` | 索引缺失 ADR-0019/0020，补全；ADR-0016 日期空白修复；完善 0016 标题
- `decisions/ADR-0008` | L3 沙箱描述"gVisor"与 M07 §4.1 三平台实现（Firecracker/VZ.framework/WSL2）不符，全面修正；Tier-2+ 改为 Tier-1+
- `decisions/ADR-0019` | 背景中历史草案编号（023/026/027/028）与当前 DDL 目录（001-021）不符，补注说明已重整

**ADR 新增**：

- `decisions/ADR-0020` | 确立 DeepSeek V4 为全系统默认核心模型（Canonical Provider）；开放后台认知任务频率限制；本地模型重定位为延迟极限/物理隔离/灾备特权

**docs/specs/ 规范修订**：

- `02-Rust-FFI.md RUST-4` | 依赖白名单补充 `surrealdb`（ADR-0010 已引入），明确"新增须记录 ADR"
- `03-Agent-Pattern.md AGENT-4` | Wasm 加载描述修正：`EmbedWasmLoader` 从 embed.FS 加载（而非直接文件系统读取），消除歧义
- `04-Module-Boundary.md B1` | `internal/` 层描述逻辑倒装修正（原文语义相反）
- `04-Module-Boundary.md B5.3` | 删除过时的 `cgo` 引用（ADR-0011 已完成全量 purego 迁移）
- `06-Review.md C9` | R8 引用不精确，明确 diff ≤300 行来源
- `07-Reference-Implementation.md §7.1` | 删除 `adapter_anthropic.go` 重复 canonical 条目（行 14 与行 27 重叠）
- `08-Doc-Hygiene.md` | 对象范围补充 `M13-bis-Extension-Registry.md`
- `INDEX.md 守则4` | "doc↔代码冲突"描述精确为"规约文档（docs/arch/ + docs/specs/）与代码冲突以规约为准"

## 2026-06-02（扩展系统全局架构重构）

**架构决策变更（破坏性）**：

- `docs/arch/decisions/ADR-0019` | 推翻"插件子组件不跨边界注入全局表"设计，改为 agentskills.io 标准：安装时子 MCP 写 `mcp_servers`（加 `plugin_id`+`work_dir`），子 Skill 写 `skills`（加 `plugin_id`），生命周期级联管理
- `docs/arch/M13-bis-Extension-Registry.md` | §1/§2/§5.3/§5.7/§6.3/§12 全面更新：Plugin Bundle 安装流重写、卸载流重写、API 表更新、表引用速查补充 FK 关系
- `docs/arch/M06-Skill-Library.md §9` | AgentSkills 标准适配章节完整重写：补 exec_mode 完整 frontmatter、skill name 命名规范（独立/插件/内置三种格式）、plugin_id 级联说明

**DDL 变更（破坏性，需删库重建）**：

- `internal/protocol/schema/015_mcp_servers.sql` | 新增 `plugin_id TEXT NOT NULL DEFAULT ''` + `work_dir TEXT NOT NULL DEFAULT ''`
- `internal/protocol/schema/020_extension_instances.sql` | 删除 `enabled`（废字段）、`parent_id`（死代码）两列
- `internal/protocol/schema/021_plugins.sql` | 注释更新，反映 mcp_policy 仅存附加策略
- `internal/protocol/schema/008_skills.sql` | 新增 `plugin_id TEXT NOT NULL DEFAULT ''`

**代码变更摘要**：

- `pkg/extensions/mcp/mcp_manager.go` | 删除 `LoadFromPlugins` / `LoadOnePlugin` / `readFileBytes`；`LoadFromDB` 读 `work_dir`，注入 `MCPClientConfig.WorkDir`
- `pkg/gateway/server/mcp_servers.go` | 删除 `appendPluginMCPServers`；`handleListMCPServers` 改 LEFT JOIN；`DELETE/PUT` 对插件 MCP 返回 405；`startMCPServerCtx` 传入 WorkDir
- `pkg/gateway/server/plugin_catalog.go` | 安装插件时调 `registerPluginMCPServers`（写 mcp_servers）；独立 skill 改 `skill:{hex}` 命名，统一走 `skillReg.Register`；删 `enabled` 引用
- `pkg/gateway/server/plugin_manage.go` | 完整重写：`handleListPlugins` 从 mcp_servers 读状态；`handleUpdatePlugin` 级联同步；`handleTogglePluginMCP` 操作 mcp_servers.enabled
- `pkg/extensions/marketplace/manager.go` | 补 `case "app"` 删 apps 表；独立 skill 卸载改硬删；`removePluginRuntime` 从 mcp_servers 读 ID，级联硬删；删 `OR parent_id=?` 死引用
- `pkg/gateway/server/plugin_custom.go` | `handleCreateApp` 写 apps 表；全量删 `enabled` 引用
- `pkg/cognition/kernel/agent.go` | 修复 `refreshInstalledExtensions`：删不存在的 `version`/`parent_id = ''` 列
- `pkg/cognition/skill/seeder.go` | 删 `enabled`/`parent_id` 死列引用
- `internal/protocol/interfaces.go` | `SkillMeta` 新增 `PluginID string`
- `pkg/cognition/skill/sqlite_registry.go` | Register/Get/List 支持 plugin_id

## 2026-05-23（初始化链路重构）

**BUG 修复**:
- `cmd/polaris/main.go` | schema 加载从相对路径 OpenSQLiteFromDir 改为 embed.FS OpenSQLite，消灭已安装二进制启动失败
- `internal/config/config.go` | Threshold 加载从 config.Load() 内剥离为独立 LoadThresholds(dataDir)，解决 chicken-and-egg；TOML 默认路径改为 ~/.polarisagi/polaris/config/
- `skills/builtin` | Wasm 加载从 FilesystemWasmLoader 改为 EmbedWasmLoader，impl.wasm embed 进二进制
- `configs/defaults.toml` | interface.host 从 0.0.0.0 修正为 127.0.0.1，符合 ARCHITECTURE.md §1 硬约束
- `pkg/substrate/observability/` | SurrealDB Core 启动条件改为 autoConf != nil &&，防止硬件未知时 OOM

**安全**:
- `cmd/polaris/cli.go` | initPromptSecret 改用 term.ReadPassword 屏蔽 API Key 回显

**可观测性**:
- `internal/config/config.go` | loadModuleTOML 错误不再静默吞噬：文件不存在 Debug，解析失败 Error + Fail-Fast
## 2026-05-22（集成接口规范 + DB 写路径澄清）

**规范新增**：
- `docs/arch/00-Global-Dictionary.md §1-ter` | 新增 XR-08（日志规范）、XR-09（LLM 调用）、XR-10（工具/技能/插件执行）、XR-11（文件系统操作分层）；更新 XR-04（DB 写路径三层规范澄清）；更新 `[Storage-SQLite]` 条目
- `docs/specs/00-Constitution.md §R1` | 新增反模式 R1.11（绕过 Provider）、R1.12（直接打印）、R1.13（绕过沙箱执行命令）
- `docs/specs/01-Go-Code.md` | 新增 F8（日志规范+必选 key 约定+级别表）、F9（HTTP Handler 四段式）、F10（Context 传播+deadline 规范）
- `docs/specs/07-Reference-Implementation.md §7.1` | 新增 canonical：HTTP Handler（channels.go）、LLM 调用（adapter_anthropic.go）、MutationBus 写（mutation_bus.go）、Store 同步写（store.go§Put/Txn）

**代码修复**（同日）：
- `cmd/polaris/main.go` | 接入 MutationBus（DatabaseWriter + EventLog + DecisionLog），修复 MutationBus 从未运行的架构断层；添加优雅退出等待 flush
- `pkg/swarm/sqlite_blackboard.go` | 修正注释（删除"委托 MutationBus"的错误声明，说明 CAS 需要直接写的原因）
- `pkg/substrate/mutation_bus.go` | 修正适用范围注释
- `pkg/substrate/storage/store.go` | 修正 Put/DB() 注释，澄清三层写路径定位

**背景**：AI 编程大模型在以下场景无规范可依，导致生成代码跑偏：(1) 数据库写路径（MutationBus/Store.Put/裸SQL 各自适用场景不清）；(2) 日志（`fmt.Printf` 与 `slog` 混用）；(3) LLM 调用（绕过 Provider 直接构造 HTTP）；(4) 工具/技能执行（绕过 ToolRegistry 直接调用具体实现）；(5) HTTP Handler 结构（SQL 内嵌 handler）。本次规范补全覆盖以上全部缺口。

## 2026-05-22（DDL 修改策略 + Schema 整合）

**规范新增**：
- `CLAUDE.md §编码约定` | 新增 `[强制] DDL 修改策略`：上线前直接修改建表文件，禁止 ALTER TABLE 补丁；上线后走编号迁移文件；Phase 判断 SSoT 为 `§当前阶段`
- `05-Coding-Workflow.md` | 新增 W7（Schema 变更流程）：W7.1 Phase 判断 → W7.2 上线前直改 → W7.3 上线后迁移 → W7.4 阶段 A 契约补充

**背景**：AI（Gemini）反复以 ALTER TABLE 补丁文件叠加 Schema 变更，造成 026_skills.sql 死代码、双写冗余、`getInstalledCatalogIDs` 需 UNION 五表等结构性问题。规则缺失是根因。本次同步完成 35→20 文件 Schema 整合（新增 M13-bis / ADR-0019 / extension_instances SSoT）。

## 2026-05-22（文档卫生规约）

**规范新增**：
- `08-Doc-Hygiene.md` | 新增 docs/arch/ 维护边界。H1 三层判定（契约/决策/实现）+ H2 修饰物清理 + H3 数值双写消除 + H4 EntryPoint 化前置条件 + H5 决策迁 ADR + H6 Tier C 禁区 + H7 锚点化 + H8 五条验收门 + H9 Pilot 协议
- `INDEX.md` | 加载策略表新增第 08 行（改架构文档前加载）

**背景**：评估外部 Schema-first 极简方案后，确认全盘 EntryPoint 化会摧毁 PII/Taint/Capability 顺序契约（违反 [HE-Rule-5]/[HE-Rule-6]）。改采"差分式精简"——清修饰、消双写、保契约。首次 Pilot 选 M04。

**Pilot 反馈（同日修订 H8）**：
- M04 Pilot 实跑显示，契约密集型文件做完合规 A1+A2 后字符量微增（+0.76%）——A2 下推让 `spec/state.yaml §m4_kernel.xxx` 引用比裸数字更长
- H8 门 1 原"目标 -15%~-25%"假设所有 M_X 等价，实测假设错误
- 修订：门 1 改为"行动度量优先（A1≥1 + A2 全覆盖）+ 文件类型分级 token 参考值"。契约密集型 -5%~+5%，平衡型 -8%~-18%，实现密集型 -15%~-25%
- 价值：暴露规约缺陷正是 Pilot 的目的，符合 H9 协议

## 2026-05-16（规范体系初始化）

**规范规则新增**：
- `00-Constitution.md` | 新增 R7（可读性硬上限：函数≤60行/文件≤400行/嵌套≤3/圈复杂度≤15）
- `00-Constitution.md` | 新增 R8（参考实现强制引用：写新代码前必须 Read canonical 标杆）
- `04-Module-Boundary.md` | 新增 B5（契约版本化与破坏性变更协议）
- `05-Coding-Workflow.md` | W2 前置 Stage 0（上下文锚定），新增 W6（PR 纪律：原子变更/契约分离/PR 描述模板/对抗审查）
- `06-Review.md` | 新增 C8（参考实现对齐）、C9（PR 体积检查）

**参考实现体系建立**：
- `07-Reference-Implementation.md` | 新增标杆代码索引，全部 `pkg/` 的 canonical 文件确认（见表）
- `pkg/*/AGENTS.md` | 6 份模块级 AI 上下文文件（substrate/cognition/action/swarm/governance/edge）

**支撑体系建立**：
- `../arch/00-Global-Dictionary.md` | 新增 §13 标识符↔概念映射表（命名一致性 SSoT）
- `../arch/decisions/` | 新建 ADR 目录，初始化 ADR-0001~0014（依赖选型回填 + R1.3/R1.4/lint/对抗审查决策）
- `../arch/spec/state.yaml` | 补 `s_interrupt` 状态（spec_consistency_test 发现 Go↔yaml 漂移）
- `.golangci.yml` | 启用 4 个规范 linter（depguard/errorlint/nestif/gocyclo）
- `.github/workflows/constitutional-review.yml` | PR 触发对抗审查 GitHub Action

**ADR 执行状态**（代码已落地，记录于各 ADR 修订记录）：
- ADR-0002：skill.go 本地接口/类型全部删除，统一 protocol.SkillMeta（-~200行死代码）
- ADR-0011：cedar_ffi.go + surreal_store.go 完成 cgo→purego 迁移，ABI 1.0 协议
- ADR-0012：spec_consistency_test.go 落地，4 项 Tier 1 SSoT 守护
- ADR-0013：.golangci.yml 启用 4 linter，CI fail-closed
- ADR-0014：constitutional-review.yml + scripts/constitutional_review.sh 落地
