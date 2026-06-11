# ADR-0015: Codex 特性集成

**状态**: Accepted（§2.1 Plugin 层定位已被 ADR-0016 取代；其余章节有效）  
**日期**: 2026-05-21  
**作者**: AI Architect  
**背景 PR**: —

---

## 1. 背景

OpenAI Codex 官方文档定义了 AI Agent 操作系统能力的行业标准共识：**Plugin = Skills (可复用工作流) + App Integrations (外部应用接入, 即 MCP)**。
在这一共识下，Codex 在七个维度定义了能力边界：Plugin（聚合分发载体）、MCP 增强（OAuth/per-tool 审批）、Skills（agentskills.io 标准 SKILL.md 格式）、Subagents（TOML 自定义 Agent + CSV batch fan-out）、Hooks（事件驱动脚本注入）、Rules（prefix_rule DSL）、Permissions（文件系统+网络权限 profile）。

Polaris 已有 M6（技能库）、M7（工具/MCP/沙箱）、M8（多 Agent 编排）、M11（Cedar 策略/安全）覆盖了对应的底层能力，但缺少以下用户面层：

| 能力 | Codex | Polaris 现状 | 差距等级 |
|---|---|---|---|
| Plugin 分发单元 | Plugin manifest + marketplace | 分散注册，无 bundle | **P1 缺失** |
| 生命周期 Hook | hooks.json 脚本注入 | ARCHITECTURE.md 声明意图，无实现 | **P1 缺失** |
| SKILL.md 标准格式 | agentskills.io 标准 | 自有 SkillDef 格式 | **P2 不兼容** |
| Custom Agent TOML | .codex/agents/*.toml | AgentCard 无 YAML 定义路径 | **P2 缺配置层** |
| CSV batch fan-out | spawn_agents_on_csv | Blackboard+CAS 存在，无 CSV 入口 | **P2 缺编排模式** |
| prefix_rule DSL | rules/ DSL | Cedar（功能更强） | **P3 UX 差距** |
| Permission Profile | filesystem+network profile | Capability Token + 三级沙箱 | **P3 配置层缺失** |

---

## 2. 决策

### 2.1 Plugin Registry（P1）

> **⚠ 本节已被 [ADR-0016](./ADR-0016-unified-trust-extension-model.md) 取代。**  
> ADR-0016 发现 `pkg/gateway/server/plugin_catalog.go`（M13）已是 Plugin Catalog 的正确位置，并将 Plugin 安装 API 统一至 M13 `/v1/plugins/`。Plugin 物理结构、Manifest 格式、TrustTier 体系以 ADR-0016 §2.3 为准。本节保留供历史参考。

**决策（已被修正）**: ~~在 M7（Action 层）新增 `pkg/action/plugin/` 子包作为 Plugin Registry。~~ → Plugin Catalog 在 M13，M7 提供底层基础设施（详见 ADR-0016）。

**原始理由**: Plugin 的核心职责是 MCP Server 配置 + Skill 注册的聚合分发，两者都在 M7 管辖。Plugin 加载后将 Skills 委托给 M6、MCP 委托给 M7 现有 MCPManager，避免横切其他层。

**Plugin 物理结构与 Manifest 格式**（完全照搬 Codex 官方规范）:

我们采用与 Codex 完全一致的多文件解耦标准目录结构，通过根入口映射各项子能力：
```text
my-plugin/
  .polaris-plugin/
    plugin.json      # 核心元数据与分发入口
  .mcp.json          # MCP Server 配置声明
  .app.json          # App UI Widgets 与连接器映射
  skills/
    my-skill/
      SKILL.md       # Skill 认知指令全文
  hooks/
    hooks.json       # 生命周期 Hook 配置
```

**入口 Manifest 示例** (`.polaris-plugin/plugin.json`):
```json
{
  "name": "browser",
  "version": "1.0.0",
  "description": "Control the in-app browser with Codex",
  "skills": "./skills/",
  "mcpServers": "./.mcp.json",
  "apps": "./.app.json",
  "hooks": "./hooks/hooks.json",
  "interface": {
    "displayName": "Browser",
    "category": "Productivity"
  }
}
```

**挑战 A**（架构冲突）: Plugin 是横切关注点，本应独立层。反驳：Plugin 在 Polaris 中不是用户交互层（那是 M13），而是工具能力的分发载体，放 M7 是最小变更路径。待 Plugin 规模扩大后，可提升至 M13（ADR 修订）。

### 2.2 Hook 框架（P1）

**决策**: 在 `pkg/action/hook/` 实现 ShellHook 执行引擎，从 `~/.polarisagi/polaris/hooks/hooks.yaml` 加载配置。

**Hook 事件映射**:
| 事件 | 触发点 | 模块 |
|---|---|---|
| `SessionStart` | Gateway 建连 | M13 |
| `PreToolUse` | sandbox 执行前 | M7 |
| `PostToolUse` | sandbox 执行后 | M7 |
| `UserPromptSubmit` | 消息入队 | M13 |
| `Stop` | FSM → S_IDLE | M4 |

**挑战 B**（安全边界）: Codex Hook 允许外部脚本任意修改 Agent 行为，违反 HE-Rule-2（可验证执行）。

**解法**: Hook 脚本输出强制 TaintLevel=High 封装为 TaintedString，通过现有 PolicyGate（M11 Cedar）决定是否允许注入 Agent 上下文。Hook 输出只能进入 MutableSkill Zone，禁止进入 Immutable Zone（System Prompt 核心）。Hook 执行超时 30s，失败不中断主流程（可观测但不阻断）。

**挑战 C**（Hook 并发模型）: Codex 多个匹配 Hook 并发执行。Polaris 采用 errgroup 并发，任一超时不影响其他，全部完成（或超时）后主流程继续。

### 2.3 agentskills.io 标准适配（P2）

**决策**: 在 `pkg/cognition/skill/` 新增 `agentskills_adapter.go` 将 SKILL.md 格式转换为 `protocol.SkillMeta`。

**挑战 D**（cosign 签名缺失）: agentskills.io SKILL.md 无 SIGNATURE 文件，Polaris Register() 要求 `SignatureValid=true`。

**解法（已被 ADR-0016 修正）**: ~~适配器对外部 SKILL.md 生成本地 HMAC-SHA256 签名，设置 `SignatureValid=true` 并附加 `trust:local` 标签。~~ → ADR-0016 §2.1 以 `TrustTier` 枚举替代 `SignatureValid bool`；外部 SKILL.md 对应 `TrustLocal(1)` 或 `TrustCommunity(2)`，Cedar 策略通过 `TrustTier.MaxSandboxTier()` 约束沙箱级别。

**挑战 E**（progressive disclosure 语义）: Codex Skills 的 "初始只加载 name+description，按需加载全文" 与 Polaris SkillMeta 设计一致（SkillMeta 不含 SKILL.md 全文）。适配器只解析 frontmatter 生成 SkillMeta，全文 SKILL.md 按需读取（已有 wasm_loader.go 的懒加载模式）。

### 2.4 Custom Agent YAML（P2）

**决策**: 在 `pkg/swarm/` 新增 `agent_profile.go` 支持从 `.polaris/agents/*.yaml` 加载自定义 Agent 配置，映射到现有 AgentCard。

**格式**（与 Codex TOML 对应，Polaris 用 YAML）:
```yaml
name: pr_explorer
description: "只读探索 Agent，用于 PR 代码路径映射"
instructions: "探索代码，追踪调用链，禁止修改文件"
model: deepseek-v4
sandbox_tier: 1   # read-only → Sbx-L1
max_depth: 1
skills: []
mcp_servers: []
```

**挑战 F**（max_depth 递归防控）: Codex 默认 max_depth=1 防无限递归。Polaris M8 当前无深度计数。解法：TaskEntry 注入 `SpawnDepth int`，PostTask 时检查 `SpawnDepth ≥ AgentProfile.MaxDepth` 则拒绝，错误冒泡至父 Saga。阈值默认 1，通过 state.yaml `agents.max_depth` 配置。

### 2.5 CSV Batch Fan-out（P2）

**决策**: 在 `pkg/swarm/` 新增 `csv_fanout.go` 实现 CSV 输入 → 批量 Task → Blackboard → 结果聚合。

**挑战 G**（State-in-DB，HE-Rule-6）: Codex 用独立 SQLite（`sqlite_home`）存储 job 状态。Polaris HE-Rule-6 要求所有持久状态走 M2 EventLog。解法：每行 Task 的状态变更写入 EventLog（`event_type=csv_job_row_*`），结果用 Blackboard 的 `task.Result` 字段存储，无独立 SQLite，导出时从 EventLog 重建 CSV。

### 2.6 LLM Auto-Generation (Skill-Creator) (P1)

**决策**: 废弃用户手动编写模板的假设，在 `pkg/swarm/self_improve/` 下新增 `skill_creator.go` 机制。

**理由**: 对标 Codex `$skill-creator`，技能和插件不应由人类手写。Polaris 内部注册一条特权系统级指令，在会话中与用户对话获取意图后，大模型基于已扩充的 L1 原生工具层（如 `str_replace_editor`, `multi_edit`, `run_command`），直接对物理文件系统进行精准的自动化读写，生成标准的 `SKILL.md`（带 name/description 元数据前缀）和 `.mcp.json` 规范化包。这种原生工具层的支撑消除了对历史受限 Wasm 技能进行底层文件操作的依赖。

### 2.7 Marketplace Integration (市场协议接入) (P1)

**决策**: 在 `pkg/action/plugin/` 下新增 `marketplace.go`，支持检索与自动安装。

**理由**: 现代 Agent 必须能连接外部应用生态。我们将接入官方的 MCP Registry (`registry.modelcontextprotocol.io`) 及社区聚合平台（如 `mcp.so`）。系统提供统一的 API 封装：`SearchMarketplace(query)` 与 `InstallExtension(pkgID)`。大模型可通过调用系统工具直接向市场查阅可用 App / Plugin，读取安装配置指令后，由系统全自动下载并写入本地 `plugin.json`。

### 2.8 不做（P3，本 ADR 范围外）

- **prefix_rule DSL**: Cedar 已覆盖且更强，新增 DSL 引入双策略引擎风险（[ADR-0005] Cedar 是唯一策略执行器）。如需 UX 改善，在 M13 UI 层提供 prefix_rule → Cedar 策略生成器。
- **Permission Profile**: 需要 OS 沙箱扩展（macOS sandbox / Linux seccomp），与 M7 三级沙箱体系集成工作量大，列入 ROADMAP。
- **MCP OAuth**: M7 MCPClient 需要完整 OAuth 流程，涉及 M13 回调端点，独立为后续 ADR。

---

## 3. 不变量验证

| HE-Rule | 影响 | 验证 |
|---|---|---|
| R1 可观测 | Hook 执行写 TokenBurnRate 事件 | Hook runner 记录执行时长+结果状态到 EventLog |
| R2 可验证执行 | Hook 输出 TaintLevel=High | PolicyGate 在 Hook 输出注入前强制检查 |
| R3 可组合原语 | Plugin 内 MCP/Skill 走现有协议 | Plugin loader 用 MCPManager.Add + skill.Registry.Register |
| R4 数据驱动 | CSV fan-out 结果入 EventLog | Eval Harness 可消费 csv_job_* 事件 |
| R5 状态机控制流 | Hook 不得直接转移 FSM 状态 | Hook 输出类型限定为 string，不含 FSM 事件 |
| R6 State-in-DB | CSV job 状态写 EventLog，不起新存储 | csv_fanout.go 依赖 EventLog，禁直接文件写 |

---

## 4. 被拒绝的方案

| 方案 | 拒绝原因 |
|---|---|
| 完全采用 Codex TOML 格式替换 Polaris 配置 | 破坏现有 YAML 配置惯例，双格式维护成本高 |
| Plugin 放在 M13（接口层） | ~~M13 负责 HTTP/UI，Plugin 的核心是工具能力分发，属 M7~~ → **已被 ADR-0016 推翻**：Plugin Catalog 正确位置是 M13（`plugin_catalog.go`），M7 提供底层基础设施 |
| Hook 直接修改 System Prompt Immutable Zone | 违反 [ContextAssembler] Immutable Core 不变量（M5 §2） |
| CSV job 用独立 SQLite | 违反 HE-Rule-6，Polaris 不允许引入额外存储进程/文件 |
| 引入 prefix_rule 作为第二策略引擎 | 违反 [ADR-0005]，Cedar 是唯一策略执行器 |

---

## 5. 关联文档

- [ADR-0002] Skill Registry 合并 → 本 ADR Plugin/Skill 适配依赖其决策
- [ADR-0005] purego FFI Cedar → prefix_rule 不引入，理由援引本 ADR
- [ADR-0008] 三级沙箱 → Hook 执行在 L1（InProc），Plugin MCP 在 L1/L2
- **[ADR-0016]** 统一信任-扩展模型 → 取代本 ADR §2.1 Plugin 层定位；TrustTier 取代 SignatureValid bool；Plugin Catalog 移至 M13
- M06 §agentskills 适配, **M13 §Plugin Catalog**（非 M07；见 ADR-0016）, M07 §Hook 框架, M08 §Custom Agent, M08 §CSV fan-out
