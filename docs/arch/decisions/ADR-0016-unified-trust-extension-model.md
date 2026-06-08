# ADR-0016: 统一信任-扩展模型（最终版）

**状态**: Accepted  
**日期**: 2026-05-21  
**作者**: AI Architect  
**取代**: ADR-0015 §2.1（Plugin 层定位）中间方案  
**相关**: ADR-0002, ADR-0005, ADR-0007, ADR-0008

---

## 1. 背景与核心发现

通过全局代码审查发现三个关键事实，彻底改变了 ADR-0015 的部分前提：

**发现 1**: `skills/builtin/shell_exec/SKILL.md` 证实**内置技能已经是 agentskills.io 标准格式**。格式统一问题**已经解决**，无需迁移。真正的问题是信任层太粗（`SignatureValid bool` 只有真/假）。

**发现 2**: `pkg/gateway/server/plugin_catalog.go` 证实 **M13 已有 Plugin Catalog（MCP 专属）**，这是 Plugin Registry 的正确位置。ADR-0015 把 Plugin Registry 放 M7 是临时错误决策。

**发现 3**: `AgentCard.TrustLevel int` 已有信任字段但无类型定义，`SignatureValid bool` 语义过于粗糙——无法区分「本地签名」「官方签名」「系统内置」。

**真正要解决的问题**：
```
格式    → 已解决（SKILL.md 已是事实标准）
定位    → 已解决（M13 plugin_catalog.go 是正确位置）
信任    → 未解决（bool 无法区分五种来源）
三大平台 → 未解决（无 publisher 概念，无官方白名单）
```

---

## 2. 决策：五级信任体系

### 2.1 TrustTier 替代 SignatureValid bool

```
TrustSystem   (4) → Polaris 内置，硬编码                 → Sbx-L2/L3, TaintNone,   approval=auto
TrustOfficial (3) → OpenAI/Google/Anthropic/MCP 官方      → Sbx-L2,   TaintMedium, approval=auto
TrustCommunity(2) → cosign 签名，publisher 未认证         → Sbx-L1,   TaintHigh,   approval=prompt
TrustLocal    (1) → HMAC 本地签名，实例密钥               → Sbx-L1,   TaintHigh,   approval=prompt
TrustUntrusted(0) → 无签名 → REJECT (fail-closed)
```

**关键决策**：TrustOfficial = TrustSystem - 1，即**三大平台官方技能与内置技能等权限**，无需用户额外审批（`approval=auto`）。这是用户明确要求，也符合最小权限原则（非 TrustSystem，不能用 Sbx-L3）。

### 2.2 官方 Publisher 白名单

预置受信 Publisher 列表（`configs/trusted-publishers.yaml`），离线验证方案：

| Publisher | GitHub Org | 原因 |
|-----------|-----------|------|
| modelcontextprotocol | modelcontextprotocol | Anthropic 主导的 MCP 官方组织 |
| anthropic | anthropics | Anthropic 官方 |
| openai | openai | OpenAI 官方 |
| google | google | Google 官方 |
| github | github | GitHub/Microsoft 官方 |
| microsoft | microsoft | Microsoft 官方（Playwright 等） |
| figma | figma | Figma 官方 |

**验证策略**（双模式）：
- **离线（默认）**：Publisher ID 白名单 + content hash pinning（`official-registry.yaml`）
- **在线（可选）**：cosign + Sigstore OIDC（GitHub Actions OIDC，`subject_prefix` 匹配）

**版本锁定**：`official-registry.yaml` pin 到 commit hash + content sha256，Operator 手动审核升级（类 Homebrew formula），禁止自动升级（供应链风险）。

### 2.3 Plugin 正式定义

```
Plugin = {
  skills:         []SKILL.md   # agentskills.io 标准，TrustTier 由 publisher 决定
  mcp_servers:    []MCP Config # transport + command/URL + TrustTier
  agent_profiles: []YAML       # Custom Agent 定义
  hooks:          hooks.yaml   # 生命周期脚本注入
}
```

Plugin 全局 ID 格式：`{publisher}:{name}@{version}`，如 `openai:github-skills@1.0.0`。

Plugin 安装位置：M13 `/v1/plugins/` API（扩展现有 Catalog）。

### 2.4 Skill 格式最终确认

**agentskills.io SKILL.md 是唯一权威来源格式**（内外统一）：

```
skill-name/
├── SKILL.md          # frontmatter: name, description + 使用指令（必须）
├── impl.wasm         # Logic Collapse 产物（可选，存在时优先执行）
├── SIGNATURE         # cosign 签名（内置/官方）或 HMAC（本地）
└── agents/
    └── polaris.yaml  # 元数据扩展（可选）
```

**Polaris 原生「扩展字段」**（`agents/polaris.yaml`，对应 Codex `agents/openai.yaml`）：
```yaml
policy:
  allow_implicit_invocation: true  # 隐式调用控制
  trust_tier_override: null        # 通常由 publisher 白名单决定，不允许自覆盖
sandbox:
  tier: 1                          # 最大 Sbx 级别（受 TrustTier 上限约束）
dependencies:
  tools:
    - type: mcp
      value: github-mcp
```

---

## 3. 挑战与解法

### 挑战 1：SignatureValid bool → TrustTier（proto-break）

**影响**：`protocol.SkillMeta.SignatureValid bool` 有 6 处引用（4 处在 skill 包，2 处在 plugin 包）。数据库 `008_skills.sql` 有 `signature_valid BOOLEAN` 列。

**解法**：
- 添加 `021_skill_trust_tier.sql` 迁移：ADD `trust_tier INTEGER`，从 `signature_valid` 保守迁移（`true → TrustCommunity(2)`）
- 新代码只写 `trust_tier`，旧 `signature_valid` 列保留（向后兼容读取），不再写入
- 下次内置技能 UPSERT 时，会以 `TrustSystem(4)` 覆盖，无需手动升级

### 挑战 2：三大平台「官方」的边界

**问题**：OpenAI 官方 MCP server 极少（主要是 Docs MCP），Google 尚无公开 MCP server，Anthropic 通过 modelcontextprotocol org 维护。「三大平台」实际在 MCP 生态中贡献不均等。

**解法**：
- 不强行分三家，而是以「官方 Publisher 白名单」为准
- 当前 TrustOfficial Publisher：modelcontextprotocol（Anthropic 主导）、openai（Docs MCP）、github（OpenAI Codex 推荐）、microsoft（Playwright）、figma（设计工具）
- Google 官方 MCP server 发布后，Operator 通过升级 `official-registry.yaml` 引入
- 用「Publisher 白名单」代替「三家公司」，更有可扩展性

### 挑战 3：官方技能更新 vs 供应链安全

**问题**：外部官方技能更新频繁，自动跟进引入供应链风险，手动跟进带来运维负担。

**解法**：`configs/official-registry.yaml` pin 所有官方入口版本（commit hash + sha256）。Polaris 启动时检查本地 pin 与远程 latest，若不一致则 WARN 日志（类 `brew outdated`）。**不自动升级**，Operator 手动 review + 更新 pin。

### 挑战 4：MCP TrustTier 与 Taint 的映射

**问题**：`MCPClient.Trusted bool` 控制 TaintLevel，无法区分 TrustOfficial 和 TrustCommunity。

**解法**：
- `MCPServerConfig` 增加 `TrustTier int` 字段（数据库 `022_mcp_trust_tier.sql`）
- `MCPClientConfig.Trusted bool` 由 `TrustTier >= TrustOfficial` 派生，无需修改 MCPClient 接口
- `mcp_servers` 表现有 `catalog_id` 非空的条目（官方推荐安装）自动迁移到 `trust_tier=3`

### 挑战 5：Plugin Bundle 与现有 Catalog 的关系

**问题**：`CatalogEntry` 当前只描述 MCP server，Plugin bundle（含 Skills+MCP+Hooks）是新概念。

**解法**：
- `CatalogEntry` 增加 `Type` 字段（`"mcp"` | `"skill"` | `"plugin"`）和 `Publisher`、`TrustTier` 字段
- 现有 MCP 条目默认 `Type="mcp"`，新增 Skill 和 Plugin bundle 条目
- Plugin bundle 安装 API 新增 `POST /v1/plugins/bundle/install`（与单 MCP 安装区分）

### 挑战 6：Progressive Disclosure 的 8000 字符预算

**问题**：当大量官方 Skills 入库后，SkillSelector 初始列表可能超过 8000 字符上下文预算。

**解法**：
- SkillSelector 优先列出 `TrustSystem` 和 `TrustOfficial` 技能
- 超过预算时，按 `Benchmarks.PassRate` 降序截断（最弱技能最先省略）
- 列表超额时 WARN 日志，建议 Operator 禁用低使用率技能

---

## 4. 不变量验证

| HE-Rule | 影响 | 解法 |
|---|---|---|
| R2 可验证执行 | TrustOfficial 技能在 Sbx-L2 执行 | inv_M6_03 + inv_M7_03 保持，TrustTier.MaxSandboxTier() 硬约束 |
| R2 可验证执行 | TrustUntrusted fail-closed | `Trust < TrustLocal` → 拒绝注册，与原 `!SignatureValid` 语义等价 |
| R6 State-in-DB | trust_tier 落 SQLite | 021/022 迁移脚本，无内存专属状态 |

---

## 5. 被拒绝的方案

| 方案 | 拒绝原因 |
|---|---|
| 保留 SignatureValid bool + 用 Capabilities["trust:official"] 标记 | 两套信任系统并存，语义模糊，Cedar 策略需要解析字符串 |
| 在线 cosign Rekor 验证作为主路径 | 自托管离线场景不可用（HT0 设计目标是可离线运行） |
| Google 官方 MCP 优先列入（无官方发布） | 没有官方发布的条目不应虚构，等官方发布后 Operator 手动引入 |
| Plugin Registry 独立为 M14 新模块 | 前后端已完成，新模块引入代价大；M13 已有正确位置（plugin_catalog.go） |

---

## 6. 关联文档

- ADR-0005: Cedar 是唯一策略执行器 → TrustTier 可作为 Cedar attribute 使用
- ADR-0008: 三级沙箱 → TrustTier.MaxSandboxTier() 对齐 Sbx-L1/L2/L3
- ADR-0015: Plugin/Hook/Skill/Agent 功能实现（M7 Plugin 中间方案由本 ADR 修正）
- M06 §9: AgentSkills 格式适配（确认 SKILL.md 已是标准，无需迁移）
- M07 §14,15: Plugin Registry + Hook（保留，M7 层基础设施正确）
- M13 §Plugin Catalog: 扩展 CatalogEntry（Publisher + TrustTier + Type）
