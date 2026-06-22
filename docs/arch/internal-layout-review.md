# internal/ 目录结构优化分析

> 目标：让 Gemini / Claude Code / DeepSeek 在开发时能以最少 token 定位到正确文件；
> 每个目录应表达单一、可口头描述的职责，单目录源文件数控制在 ≤15 为宜。

---

## 现状快照（仅计非测试源文件）

| 目录 | 源文件数 | 评级 |
|---|---|---|
| `gateway/server/` | **36** | 🔴 严重超载 |
| `memory/` | **29** | 🔴 严重超载 |
| `llm/` | **21** | 🟠 偏重 |
| `learning/` | **19** | 🟠 偏重 |
| `agent/` | **17** | 🟠 偏重 |
| `channel/` | **18** | 🟠 偏重 |
| `knowledge/` | **11** | 🟡 可接受 |
| `observability/` | **16** | 🟠 偏重 |
| `swarm/orchestrator/` | **14** | 🟡 可接受 |
| `security/` | **13** | 🟡 可接受 |
| `protocol/` | **15** | 🟡 可接受 |

---

## P0 — 立即拆分（严重超载）

### 1. `gateway/server/` → 拆为 4 个子包

**现状**：36 个源文件全部平铺，LLM 寻找"provider 管理"需要在 36 个文件里扫描文件名猜测。

**根本原因**：所有 HTTP handler 都用同一个 `package server`，但职责横跨聊天会话、插件管理、Provider 路由、系统运维等完全不同的域。

**建议结构**：

```
gateway/server/
  server.go          ← HTTP 服务器启动 / middleware / context / compressor / sse
  middleware.go
  context.go
  contextref.go
  compressor.go
  sse.go / sse_media.go
  logstream.go

  chat/              ← 会话与对话域
    sessions.go      (从 server/ 移入)
    transcript.go
    recap.go
    slash_commands.go
    audio.go

  plugin/            ← 插件生命周期域
    catalog.go       (plugin_catalog.go 改名)
    custom.go        (plugin_custom.go)
    manage.go        (plugin_manage.go)
    sync.go          (plugin_sync.go)

  provider/          ← LLM Provider 域
    providers.go     (从 server/ 移入)
    loader.go        (provider_loader.go)
    seed.go          (provider_seed.go)
    catalog.go       (catalog.go — Extension Catalog, 注意与 plugin/catalog 区分命名)

  sysadmin/          ← 系统运维域（所有非对话、非插件的管理接口）
    doctor.go
    system_update.go
    export.go
    budget.go
    insights.go
    preferences.go
    prompts.go
    cron.go
    vfs.go
    workflow.go
    channels.go
    mcp_servers.go
    tools.go
    hooks.go
    openai_compat.go
    providers_extra...  (providers 的辅助逻辑)
```

**收益**：LLM 看到 `gateway/server/plugin/` 即知所有插件管理 handler 在此；`gateway/server/sysadmin/` 汇聚系统运维接口，不再需要扫描 36 个文件名。

---

### 2. `memory/` → 拆为 3 个子包

**现状**：29 个源文件，记忆类型（情节/语义/反思）、检索管道、图结构、工具类全部混在一起。

**建议结构**：

```
memory/
  memory.go          ← 顶层接口 + MemorySystem 入口
  memory_system.go
  identity.go
  platform_hints.go
  write_filter.go
  cascade_invalidator.go

  store/             ← 各类记忆的存储实现（"存什么"）
    episodic_mem.go
    semantic_mem.go
    reflection_mem.go
    sql_reflection_mem.go
    durative_mem.go
    working_mem.go
    notes_store.go

  retrieval/         ← 检索与提取管道（"怎么取"）
    retriever.go
    query_classifier.go
    query_classifier_semantic.go
    on_demand_extractor.go
    per_message_extractor.go
    online_reindexer.go
    episodic_projector.go

  graph/             ← 记忆图结构与权重（"怎么关联"）
    episodic_graph_bridge.go
    edge_weight.go
    mmd_canvas.go
    synaptic_plasticity.go
    world_model.go
    temporal.go

  # 剩余留根层
  compressor.go      ← 上下文压缩（直接被 agent 调用，层次够高）
  consolidation.go
  simhash.go
```

**收益**：`memory/store/` 清晰表达"各类记忆的 CRUD"；`memory/retrieval/` 聚焦"如何从记忆中取出内容"；`memory/graph/` 是记忆神经图机制。三个独立关切，互不污染。

---

### 3. `llm/` → adapter 文件收入 `llm/adapter/`

**现状**：21 个源文件，8 个 `adapter_*.go` 与路由/流/凭证等基础设施混放。

**建议结构**：

```
llm/
  client.go          ← 顶层 LLMClient 接口
  router.go          ← Provider 路由器
  provider.go
  provider_route.go
  fallback.go
  stream.go
  stream_guard.go
  http_client.go
  tokenizer.go
  credential_pool.go
  error_classifier.go
  rate_tracker.go
  media_opt.go

  adapter/           ← 各 Provider 适配器（"怎么对接某个 LLM"）
    anthropic.go     (adapter_anthropic.go 去前缀)
    deepseek.go
    google.go
    ollama.go
    openai.go
    embedding.go     (adapter_embedding.go)
    steering.go
    training.go

  stt/               ← 已有，保持
  tts/               ← 已有，保持
```

**收益**：`llm/adapter/anthropic.go` 比 `llm/adapter_anthropic.go` 更易被 LLM 定位；`adapter/` 子目录明确"所有 Provider 适配器都在这里"。

---

## P1 — 文件放错目录

### 4. `tool/` 根层的沙箱文件 → `tool/sandbox/`

**现状**：`tool/` 根层有 5 个沙箱文件与工具注册/搜索逻辑混放：

```
tool/native_sandbox.go
tool/rust_native_sandbox.go
tool/rust_wasmtime_sandbox.go
tool/wasmtime_sandbox.go
tool/wasm_quota.go
```

这 5 个文件职责是"如何在沙箱里执行工具"，与 `tool.go`（工具注册）、`tool_search.go`（工具搜索）属于不同层次。

**建议**：移入 `tool/sandbox/`：

```
tool/
  tool.go            ← 工具注册、调度接口
  loader.go
  tool_search.go
  hook_runner.go

  sandbox/           ← Wasm/native 沙箱执行层
    native.go        (native_sandbox.go)
    rust_native.go
    rust_wasmtime.go
    wasmtime.go
    quota.go         (wasm_quota.go)
```

> **与 `internal/sandbox/` 的区分**：`internal/sandbox/` 是平台级沙箱（Linux namespace + 远程沙箱）；`tool/sandbox/` 是工具执行级沙箱（Wasm runtime + 原生进程调用）——两者层次不同，命名需注意在 CLAUDE.md 里说明。

---

### 5. `channel/` 的 15 个平台适配器 → `channel/adapter/`

**现状**：`manager.go`、`message.go`、`dispatch.go`（核心逻辑）与 15 个平台实现（telegram/discord/slack/...）全部平铺。

**建议**：

```
channel/
  manager.go         ← Channel 管理器
  message.go         ← 统一消息模型
  dispatch.go        ← 分发逻辑

  adapter/           ← 各平台适配器（每个文件是一个平台）
    telegram.go
    discord.go
    slack.go
    dingtalk.go
    feishu.go
    wecom.go
    qqbot.go
    signal.go
    matrix.go
    mattermost.go
    email.go
    sms.go
    teams.go
    homeassistant.go
    webhook.go
```

**收益**：新增平台只需在 `channel/adapter/` 下加文件；LLM 定位"钉钉适配器"直接看 `channel/adapter/dingtalk.go`，不用在 18 个文件里过滤。

---

### 6. `sysenv/` → 归并到 `sysmgr/sysinfo/`

**现状**：`internal/sysmgr/sysinfo/` 是独立包，提供 `SystemInfo`（OS 名称、架构、Locale、时区等静态系统信息）。包名 `sysenv` 与 `sysmgr` 命名分散，语义上属于系统管理的"探测"子域。

**建议**：将 `sysenv/*.go` 移入 `sysmgr/sysinfo/`，与 `sysmgr/downloader/` 和 `sysmgr/updater/` 并列。

```
sysmgr/
  downloader/        ← 已有
  updater/           ← 已有
  sysinfo/           ← 新建，承接 sysenv/
    sysinfo.go       (sysenv.go)
    sysinfo_unix.go
    sysinfo_windows.go
```

**影响**：所有 `import "sysenv"` 改为 `import "sysmgr/sysinfo"`，包名从 `sysenv` 改为 `sysinfo`（更清晰）。

---

## P2 — 可选优化（视开发节奏决定）

### 7. `agent/` 内部分层

`agent/` 根层 17 个文件中，部分职责可收入子目录：

```
agent/
  agent.go / agent_execute.go   ← 核心 FSM 和执行循环，保留根层
  state_machine.go / transitions.go / fallback_fsm.go
  memory_context.go             ← agent-memory 胶水层，保留根层
  prompt.go / whisper.go
  recovery.go / budget.go / epoch.go

  dag/                          ← DAG 执行与校验（"怎么并发执行子任务图"）
    executor.go    (dag_executor.go)
    validator.go   (dag_validator.go)

  # pii_vault.go 建议保留在 agent/：
  # SessionPIIVault 强依赖 protocol.SQLQuerier + agent 会话 taskID，
  # 是 agent 会话级别的加密暂存，不是全局 security 策略，放在 agent/ 合理。
```

---

### 8. `learning/` 内部分层

19 个文件可按"学习驱动力"分组：

```
learning/
  engine.go / bridge.go / calibrator.go   ← 核心引擎，保留根层
  curriculum.go / gap_fill_worker.go
  heuristics_store.go / memf.go
  logic_collapse_trigger.go
  reflection_worker.go / reflexion.go

  surprise/          ← Surprise Index 计算（SurpriseIndex + Markov）
    surprise.go
    markov.go        (surprise_markov.go)

  prompt/            ← Prompt 自动优化与版本管理
    optimizer.go     (prompt_optimizer.go)
    version_store.go (prompt_version_store.go)

  rollout/           ← 能力扩展 Rollout 与追踪
    rollout.go
    store.go         (rollout_store.go)

  synthetic/         ← 合成数据生成（Eval + Skill）
    eval_gen.go      (synthetic_eval_gen.go)
    skill_gen.go     (synthetic_skill_gen.go)
```

---

### 9. `observability/` 内部微整理

16 个文件，`hardware_probe.go` + `memory_probe*.go`（含3个平台文件）可收入子目录：

```
observability/
  metrics.go / instruments.go / record.go
  logger.go / tracer.go
  auto_config.go / cardinality_guard.go
  performance_drift.go / tier_parameters.go
  feature_gate.go
  doc.go

  probe/             ← 硬件与内存探针（跨平台）
    hardware.go      (hardware_probe.go)
    memory.go        (memory_probe.go)
    memory_darwin.go
    memory_linux.go
    memory_windows.go
```

---

## 无需改动（现状合理）

| 目录 | 理由 |
|---|---|
| `store/repo/` | 11个 repo_*.go，按业务域命名，一目了然 |
| `store/audit/` | 3个文件，职责单一 |
| `store/search/` | 4个文件，聚焦混合检索 |
| `extension/skill/` | 7个文件，技能生命周期清晰 |
| `extension/mcp/` | 4个文件，MCP 客户端完整 |
| `security/taint/` | 3个文件，污点系统独立 |
| `security/guard/` | 4个文件，内容安全守卫 |
| `swarm/orchestrator/` | 14个文件，编排模式丰富但命名规律 |
| `protocol/` | 15个文件，接口契约层，repo_*.go 定义清晰 |
| `action/` | 4个根层文件+3个子包，结构合理 |

---

## 执行优先级

```
P0（立即）: gateway/server/ 拆分 → memory/ 拆分 → llm/adapter/
P1（本周）: tool/sandbox/ → channel/adapter/ → sysenv→sysmgr/sysinfo/
P2（下个迭代）: learning/ 分层 → agent/dag/ → observability/probe/
```

**执行要点**：
- 每次移动前确认 `go build ./...` 通过
- 包名随目录同步修改（如 `adapter_anthropic.go` → `package adapter`）
- `internal/protocol/repo_*.go` 中的接口引用路径不受影响（已是接口层）
- 移动完成后补写对应目录的 `doc.go` 或 `CLAUDE.md`，让 LLM 在进入目录时立即知道该包的职责边界

---

*生成时间：2026-06-22 | 基于 370 个非测试源文件的完整扫描*
