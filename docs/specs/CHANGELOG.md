# docs/specs/ 变更日志

> 规范本身的演进记录。AI 每次会话开头扫描最近 5 条以感知规范增量。

格式：`YYYY-MM-DD | 文件 | 变更摘要`

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
