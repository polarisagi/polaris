# ADR-0016: 扩展/插件系统治理合集（统一信任模型 + Codex 特性集成 + 安装/升级生命周期，含原 ADR-0015/0019/0075）

- **状态**: Accepted | **日期**: 2026-05-21~05-22（升级生命周期补充 2026-07-23，合并 2026-07-28）| **模块**: M6/M7/M8/M11/M13/M13-bis

## 决策一：统一信任-扩展模型（最终版，原决策，取代原 ADR-0015 §2.1 中间方案）

**五级 TrustTier 替代 `SignatureValid bool`**：`TrustSystem(4)` 内置硬编码 → `TrustOfficial(3)` 官方 Publisher 白名单（与 TrustSystem 等权限，`approval=auto`）→ `TrustCommunity(2)` cosign 签名未认证 → `TrustLocal(1)` HMAC 本地签名 → `TrustUntrusted(0)` fail-closed 拒绝。

官方 Publisher 白名单硬编码于 `internal/config/trusted_publishers.go`（modelcontextprotocol/anthropic/openai/google/github/microsoft/figma），离线校验为主（Publisher ID + content hash pinning），版本锁定 commit hash + sha256，不自动升级（供应链风险，类 Homebrew formula 人工 review）。

Plugin Catalog 正确位置是 M13（`plugin_catalog.go`），非 M7——修正原 ADR-0015 的临时错误决策。`trust_tier` 列直接内嵌于 `008/015/019/020/021` 原始 DDL（上线前策略，未走独立迁移文件）。

## 决策二：Codex 特性集成（原 ADR-0015，§2.1/§2.3 已被决策一取代，其余章节有效）

对标 OpenAI Codex 的 Plugin/MCP/Skills/Subagents/Hooks/Rules/Permissions 能力边界，补齐 Polaris 用户面缺口：

- **Hook 框架**（`internal/action/hook/`）：仅实现 `PreToolUse`/`PostToolUse`（其余 3 事件由独立系统 `ShellHooks` 承接，两套引擎并存非重复）。Hook 输出强制 `TaintLevel=High`，经 PolicyGate 决定是否注入，只能进 MutableSkill Zone，不得进 Immutable Zone。执行经 `sandbox.CmdRunner`（`CallerType="hook"`）路由至统一沙箱，非裸 exec。
- **agentskills.io 标准适配**：`internal/extension/skill/` 新增适配器，`TrustTier`（决策一）替代 `SignatureValid bool`。
- **Custom Agent YAML**：`.polaris/agents/*.yaml` 映射到 AgentCard，`SpawnDepth` 防无限递归（默认上限 1）。
- **CSV Batch Fan-out**：状态变更走 EventLog（HE-6），不建独立 SQLite。
- **不做（P3）**：prefix_rule DSL（Cedar 已覆盖更强）、Permission Profile（OS 沙箱扩展工作量大，入 ROADMAP）。

## 决策三：extension_instances 统一安装实例表（原 ADR-0019）

新增 `extension_instances` 表（`020`），替代散落在 `skill_sources`/`plugins`/`apps`/`mcp_servers.catalog_id` 四表的安装记录，作为所有已安装扩展的单一事实来源（`ext_type`/`origin`/`catalog_id`/`runtime_id`/`install_path`/`status`）。安装状态归 `extension_instances`（Layer 1），运行参数严格拆分到 `mcp_servers`/`skills`/`plugins`/`apps`（Layer 2），插件内子组件不跨越边界污染基础表。

## 决策四：Extension Upgrade Versioning（原 ADR-0075，对决策三未覆盖的"升级生命周期"的补充）

`extension_catalog`/`extension_instances` 原无版本记录，卸载走级联硬删不适合升级场景。新增 `extension_catalog.version`（市场最新版本缓存）+ `extension_instances.installed_version`（实际安装版本），新增独立端点 `POST /v1/plugins/{id}/upgrade` 做增量升级，保留 `extension_instances.id` 与 `install_path`，不触发卸载副作用。

## 反例守护

拒绝保留 `SignatureValid bool` + Capabilities 字符串标记信任——两套信任系统并存语义模糊。拒绝在线 cosign Rekor 作主路径——离线场景（Tier-0）不可用。拒绝 Hook 直接修改 System Prompt Immutable Zone。拒绝 CSV job 用独立 SQLite（违反 HE-6）。拒绝引入 prefix_rule 作第二策略引擎。拒绝按类型分四个安装表——前端需 UNION 查询，安装状态无法单表追踪。

## 引用代码

`internal/config/trusted_publishers.go`、`internal/protocol/schema/{008,015,019,020,021}_*.sql`、`internal/action/hook/`、`internal/extension/skill/`、`internal/swarm/orchestrator/agent_profile.go`、`internal/gateway/server/plugin/{catalog,manage}.go`
