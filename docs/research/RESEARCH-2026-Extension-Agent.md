# 2026 AI Agent 扩展体系调研摘要

> 面向 AI 工具与开发者的参考文档。概念名称 + 功能摘要 + 来源链接，不展开细节实现。

---

## 一、协议与标准

### MCP (Model Context Protocol)
**来源**: https://modelcontextprotocol.io/specification/2025-11-25

- Anthropic 发布，现由 Linux Foundation / AAIF 治理
- JSON-RPC 2.0 over stdio / Streamable HTTP
- 2025-11-25 版本新增能力：
  - **Async Tasks** (`tasks/create` → `tasks/get`): 长任务非阻塞，立即返回 task_id，客户端轮询
  - **Elicitation**: Server 主动向用户请求补充输入（表单/确认）
  - **Server-side Agent Loops**: Server 可持有完整 agent 执行上下文
  - **OAuth 2.1 简化**: Client ID Metadata Document 消除手动注册
- 月下载量 110M，已成事实标准

### A2A (Agent2Agent Protocol)
**来源**: https://a2a-protocol.org / https://github.com/google/a2a

- Google 发布（2025-04），现由 Linux Foundation 托管
- 定位：Agent 间通信（MCP = 工具访问，A2A = Agent 协作）
- 技术栈：HTTP + SSE + JSON-RPC + Agent Cards
- **Agent Card** (`/.well-known/agent.json`): 声明 Agent 能力、端点、认证方式
- **Task 状态机**: submitted → working → completed/failed/canceled
- 与 MCP 互补：MCP 让 Agent 调用工具，A2A 让 Agent 调用 Agent

### Agent Skills 开放标准
**来源**: https://www.anthropic.com/engineering/building-effective-agents (Dec 2025)

- Skills = 可复用的 Prompt 模板 + 执行配置，SKILL.md 格式
- 支持厂商：Claude Code、Cursor、Windsurf、Copilot 等 16+
- YAML frontmatter 定义元数据，Markdown body 为 prompt 内容
- 两种执行模式：
  - **tool mode**: LLM 按需调用（工具名 `skill:{name}`）
  - **ambient mode**: 注入 system prompt，LLM 被动使用

---

## 二、厂商实现参考

### OpenAI Agents SDK Tool Types
**来源**: https://openai.github.io/openai-agents-python/tools/

- **FunctionTool**: Python/JS 函数包装，自动 JSON Schema 生成
- **ComputerTool**: 截图 + 鼠标键盘，browser/desktop 两种 environment
- **CodeInterpreterTool**: 沙箱 Python 执行
- **WebSearchTool**: Bing/OpenAI 搜索集成
- **HostedMCPTool**: 直连远端 MCP Server，自动 OAuth handshake
- **Agent-as-Tool** (`agent.as_tool()`): 将子 Agent 封装为工具，无需完整移交控制权
- **Handoff**: `transfer_to_agent()` 完整移交控制，Orchestrator → Specialist

### OpenAI Codex 技能系统
**来源**: https://github.com/openai/codex

- `~/.codex/instructions.md`: 全局 agent 指令
- `AGENTS.md` 文件系统注入：LLM 进入目录时自动注入上下文
- Approval policy: `auto-edit` / `full-auto` / `suggest` — 对应 Polaris 信任门控

### OpenAI Codex Automations
**来源**: https://developers.openai.com/codex/app/automations

- 触发器：cron（daily/weekly/自定义间隔）+ thread-based heartbeat
- 执行模式三种：`worktree`（隔离分支）/ `local`（直写主工作区）/ `direct`（非版控目录）
- Prompt 必须自包含：须声明成功标准与终止条件（非对话延续）
- `$skill-name` 语法在 automation prompt 内调用已安装技能
- 沙箱分级：read-only / workspace-write / full-access

### Claude Code Routines（/schedule）
**来源**: https://code.claude.com/docs/en/routines（2026-04-14 GA）

- 数据模型：`Routine = { prompt, repositories[], environment, connectors(MCP[]), triggers[] }`
- 触发器三类：
  - **Schedule**: hourly/daily/weekdays/weekly/cron（最小 1 小时），one-off 不计配额
  - **API**: `POST /routines/{id}/fire`（bearer token），支持传入 `text` 上下文（alert body 等）→ 返回 session URL
  - **GitHub**: PR/Release 事件 + author/label/branch/regex 过滤
- 执行流：触发 → 克隆仓库（默认分支）→ 启动 Cloud Session → Agent 无需审批执行 → 结果写入 `claude/` 前缀分支
- **架构启示**：`text` 字段透传外部上下文是轻量集成关键路径；Routine 本质是有状态 Agent 配置快照，非无状态脚本

### Google Agent Development Kit (ADK)
**来源**: https://google.github.io/adk-docs/

- Pipeline / Parallel / Loop Agents 三种编排原语
- `before_tool_callback` / `after_tool_callback`: 工具拦截钩子
- 内置 MCP + A2A 集成
- Agent Store: 中央化 Agent 发现与部署注册表

### Anthropic MCP SDK
**来源**: https://github.com/modelcontextprotocol/typescript-sdk

- `server.setRequestHandler(ListToolsRequestSchema, ...)`: 注册工具列表处理器
- `server.setRequestHandler(CallToolRequestSchema, ...)`: 注册工具调用处理器
- Resources / Prompts / Roots 协议原语
- Sampling API: Server → Client 反向 LLM 调用（Server 驱动 LLM）

---

## 三、架构模式

### Three-Layer Stack
**来源**: https://nevo.systems/p/skills-vs-plugins-vs-mcps (2025)

```
Skills (HOW to think)   →  prompt template, reasoning pattern
MCP    (HOW to connect) →  tool protocol, server integration  
Tools  (WHAT to execute) → function call, side effect
```
三层不互斥，Plugin Bundle = Skills + MCP + Tools 的打包单元。

### Lazy Tool Loading (ToolSearch 模式)
**来源**: OpenAI Agents SDK + 实践

- 问题：工具超过 40 个时 context 膨胀，LLM 精度下降
- 方案：只暴露 `search_tools(query)` 元工具，LLM 按需发现具体工具
- 阈值：`LazyLoadThreshold = 40`（可配置）
- 优先级：高频/高置信工具始终预加载

### Agent-as-Tool 模式
**来源**: OpenAI `.as_tool()` API / A2A Agent Card

- 将 Agent 封装为普通工具调用接口
- Orchestrator Agent 调用 Specialist Agent，不完整移交控制流
- 无需实现完整 A2A 握手，适合轻量多 Agent 协作

### Logic Collapse (System 2 → System 1)
**来源**: 内部 HE-Rules + M06 设计

- 技能从 script runtime（LLM 即兴执行）积累 → 编译为 wasm runtime（确定性执行）
- 高频、高置信的 LLM 行为固化为图执行路径
- 减少 token 消耗，提升可验证性

---

## 四、研究论文

### SoK: Agentic Skills
**来源**: https://arxiv.org/abs/2602.20867

- 系统化梳理 Agentic 技能定义、分类、评测
- 分类：Perception / Planning / Memory / Action / Communication 五类能力
- 指出 skill isolation 与 skill composition 的工程挑战

### SkillCraft: Learning Reusable Skills
**来源**: https://arxiv.org/abs/2603.00718

- 自动从轨迹（trajectory）中提炼可复用技能
- Skill Graph 概念：技能间依赖图，支持级联调用
- 与 Polaris 的 Logic Collapse 路径同向

### MCP Tool Ecosystem 大规模分析
**来源**: https://arxiv.org/abs/2603.23802

- 爬取 177,000+ MCP 工具的统计分析
- 发现：40% 工具存在重复功能，命名不一致是主要问题
- 建议：中央化注册表 + 语义去重 + 能力标签标准化

### ToolNet: 工具发现图网络
**来源**: arxiv (2025 Q4)

- 工具间关系建图，支持"工具推荐"和"组合发现"
- 查询 embedding → 最近邻工具节点 → 展开子图

---

## 五、企业实践模式

### Enterprise MCP Registry
**来源**: enterprise MCP deployments (2025-2026)

- 中央化注册表：工具元数据 + 版本管理 + 访问控制
- Trust Tier 体系：Official → Community → Local → Untrusted
- 审计日志：每次工具调用记录到 append-only 事件表
- Capability Token 细粒度授权：文件读/写/网络/危险操作分离

### Plugin Bundle 格式
**来源**: Anthropic Plugin spec + OpenAI Plugin (retired) + 实践

```
plugin.json          # 清单：name/version/ext_type/trust_tier
skills/              # SKILL.md 文件（可选）
mcp/                 # MCP server 二进制或脚本（可选）
hooks/               # lifecycle 脚本（install/uninstall/update）
agent/               # 子 Agent 定义（可选）
```
tar.gz 打包，SHA-256 完整性校验，支持 delta 更新。

### Automation as First-Class Extension
**来源**: Zapier/n8n 架构 + 2026 AI agent 实践

- 自动化 = 技能/工具 + 触发器（cron/webhook/event/manual）
- `ext_type='automation'` 与 skill/mcp 平级注册
- Trigger 类型：cron（定时）/ webhook（HTTP）/ event（内部事件）/ manual（UI 触发）
- 执行记录落 event 表，支持重放与审计

---

## 六、Polaris 实现对应关系

| 概念 | Polaris 实现位置 |
|------|----------------|
| MCP Server | `pkg/extensions/marketplace/` + `015_mcp_servers.sql` |
| Skill (script) | `pkg/cognition/skill/` + `008_skills.sql` (runtime='script') |
| Skill (wasm) | `pkg/cognition/skill/` + `008_skills.sql` (runtime='wasm') |
| Plugin Bundle | `pkg/gateway/server/plugin_catalog.go` |
| Extension 注册 | `019_extension_catalog.sql` + `020_extension_instances.sql` |
| Trust Gate | `pkg/substrate/policy/gate.go` |
| Capability Token | `pkg/substrate/policy/capability_token.go` |
| Taint Sandbox | `pkg/action/sandbox.go` + `InProcessSandbox` |
| buildToolSchemas | `pkg/gateway/server/server.go` |
| toolExec 路由 | `cmd/polaris/main.go` SetToolExecutor |
| Automation（cron/webhook/manual） | `017_automations.sql` + `pkg/gateway/server/cron.go` |
| Automation（event/github trigger） | 规划中（`trigger_type` 扩展） |
| Automation（worktree 执行环境） | 规划中（DDL 增 `env_type`，Git worktree 集成） |
| Automation（显式 Workflow DAG） | 规划中（`workflow_json`，复用 M4 dag_executor） |
| A2A Agent Card | `/.well-known/agent.json`（待实现） |
| Lazy Loading | `search_tools` meta-tool（待实现） |
| Ambient Injection | `injectSystemPrompt()`（待实现） |
| MCP Async Tasks | MCP 2025-11-25 spec（待实现） |
