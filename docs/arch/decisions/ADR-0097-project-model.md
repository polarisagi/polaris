# ADR-0097: 项目（Project）模型——会话的运行上下文容器，绑定工作目录，情景记忆项目隔离

> 状态：Accepted（决策一~五均已落地；情景记忆隔离门控 `make memory-isolation-check`） ｜ 首次：2026-09-21 ｜ 最近复核：2026-10-12
> 模块：`internal/protocol/schema/013_chat.sql`、`internal/store/repo/`、`internal/gateway/server/chat/`、`internal/gateway/session/`、`internal/agent/`、`internal/agent/context/`、`internal/tool/builtin/guard/`、`internal/memory/`、`cmd/polaris/cli_project.go`、`web/`

动因：`chat_sessions` 原为扁平表；工作区上下文协议（ADR-0088 决策三）缺「用户项目目录」输入；桌面/CLI 按目录使用（ADR-0096）；记忆检索原为全局。

## 决策一：项目 = 会话集合 + 可选工作目录 + 项目指令 + 信任开关

项目是会话的运行上下文容器，不是侧边栏分组标签。

- 表 `projects`（DDL 在 `013_chat.sql`）：`id` / `name` / `root_path`（空串 = 无目录项目）/ `instructions` / `trusted` / `archived` / 时间戳。`chat_sessions.project_id NOT NULL DEFAULT 'default'`。
- 恒有默认项目（`id='default'`，建表种入，不可删除、不可归档）；存量会话、channel 会话、Cron/Workflow/Webhook 无头会话一律落默认项目，不设特殊分支。
- 会话归属创建时确定（`EnsureSessionInProject`，`INSERT OR IGNORE`），聊天请求对已存在会话携带不同 `project_id` 时以库内为准；改归属只走 `PUT /v1/sessions/{id}/project`。Agent 按 sessionID 装配，故可由 sessionID 反查项目。
- 删除项目 = 会话迁回默认项目，不级联删会话。归档项目不接收新会话（`CreateSession` 以 `... WHERE archived = 0` 原子判定），既有会话可续聊。

## 决策二：`root_path` 的规范化与信任——授予显式、写入面收窄

沿用 ADR-0088 决策三信任模型，不放宽：

- `trusted` 默认 false：项目目录内 AGENTS.md / CLAUDE.md 走 TaintHigh 围栏（参考资料不是指令），true 才进 ZoneImmutable（clone 来的仓库内容攻击者可控）。全局 `trusted_workspace_roots` 与项目 `trusted` 取或。
- 信任判定用规范路径：写入时 `normalizeProjectRoot`（`filepath.Abs` + `EvalSymlinks`，须存在且为目录），存库规范路径，防软链绕过前缀判定。
- 写入面收窄：创建/修改/删除/改会话归属仅限 `ClientType.IsLocalTrusted()` 或 admin；channel 客户端、Webhook 无权创建项目、设 `trusted` 或改 `instructions`。
- `instructions` 是用户自撰内容，走可信通道（SOUL/偏好级），限长 8 KiB。若放开非本地客户端写入，必须先补 SystemPromptGuard 校验与审计。
- `root_path` 位置约束：不得是文件系统根、用户主目录，也不得位于敏感目录内或是其祖先（`guard.CheckScopeRoot`，与写入类工具黑名单同一清单含真实路径；根若是 `~`，读类工具只查白名单不查黑名单，`~/.ssh` 即可读）。
- 信任不随备份迁移：备份不含 `trusted`，恢复（`RestoreProject`）一律 false。

## 决策三：情景记忆按项目隔离，其余为用户全局层

跨项目串记忆同时破坏用户预期与 HE-2 信任边界。原规范「Episodic / Semantic / Graph 默认隔离」收窄如下（2026-09-21 修订，先改文档后改代码）：

| 记忆 | 归属 | 理由 |
|---|---|---|
| Episodic 事件（对话/执行轨迹） | 项目 | 项目细节（仓库内容、工具输出）所在；感知/规划原先不带会话过滤检索，A 项目片段会进 B 项目 Prompt |
| Durative 持续簇、Episodic 图节点 | 项目（随 Episodic） | 内容与来源同域 |
| Semantic 实体/关系 | 全局 | `semantic_entities UNIQUE(entity_type, name)` 是世界模型；拆分需重做信念修正与图遍历，「Go 1.26」「用户偏好中文」本应跨项目 |
| Reflection | 全局 | 策略级经验，价值在迁移 |
| 用户画像 / Core Memory / Persona / 知识库（RAG chunk） | 全局 | 用户层 / 用户导入文档 |

跨项目派生事实（Consolidation 抽取的实体）对 B 可见是有意的：已脱离对话语境且带 `taint_level`。

- 归属判定：写入时打标 `types.Event.ProjectID`（agent 写入点显式填；`EpisodicMem.Append` 未填时以 `protocol.ProjectIDFrom(ctx)` 兜底）；空 = 默认项目。会话改归属不迁移已形成的记忆。无法解析项目的 Agent（agent-0、课程学习、红队探针）以默认项目为当前项目：fail-closed，看不到带项目标记的记忆。

| # | 读取面 | 过滤点 |
|---|---|---|
| P1/P2 | 感知/规划 `ListEpisodicEvents` | `EpisodicQuery.ProjectID` → `EpisodicMem.Query` |
| P3/P4 | 感知/规划 `cognitive.FTSSearch`（SurrealDB 共享 FTS） | 命中 ID 经 `MemoryFacade.EpisodicProjectOf` 反查，他项目事件剔除；非事件命中放行（`internal/agent/context/memory_project_scope.go`） |
| P5 | L2 语义实体召回（`recallCognitiveAdapter`，`cmd/polaris/adapters_recall.go`） | `projectScopedFTS`，显式传入 ProjectID |
| P6 | `memory_search` → HybridRetriever 七路 | `SearchScope.ProjectID`；`projectScopedSource` 包装整个 DocumentSource，新增召回路只要经 DocumentSource 输出就必然被过滤 |

后台/系统读取方产出落入全局层或限会话范围，不在清单内。

- 门控：`TestProjectIsolation*`（`internal/memory/...`、`internal/agent/context`）以项目 A 写入唯一标记串、项目 B 经 P1~P6 查询均不得命中；`tools/memory_isolation_check.go`（[L-18]）运行这些用例并接入 `make lint`；`tools/lint-selftest.txt` 登记负向用例（拆掉 `projectScopedSource` 包装 → 报红）。
- 已知限制：(1) Tier0 SQL 向量路径中未回填 `event_uuid` 的历史行（`episodic_row:` 源）无法反查，按默认项目；(2) MemoryAgent 耳语只接 agent-0，扫描的 `episodic_events` 物化表无项目列，若接入会话 Agent 须先补该列。

**子 Agent 继承发起方项目**：委派子 Agent 的会话 ID 是一次性 `headless-…`（`internal/agent/pool.go`），不在 `chat_sessions`。复用共享记忆命名空间（GD-14-001，命名空间即根会话 ID）：解析顺序为本会话 → 命名空间所指会话（`internal/agent/agent_workspace_project.go`），不新增字段或协议；命名空间不是会话 ID 的场景（`curriculum_*`、CSV 扇出 job ID）按默认项目。命名空间由 Worker 在 `Run()` 外的 goroutine 注入，解析读其原子副本，并在意图到达时补刷一次。

## 决策四：客户端契约

- REST：`GET/POST /v1/projects`、`GET/PUT/DELETE /v1/projects/{id}`、`GET /v1/sessions?project_id=`、`PUT /v1/sessions/{id}/project`；聊天请求带可选 `project_id`。
- CLI 与桌面外壳能力对齐（ADR-0096 决策一），外壳不新增业务 Tauri command。`polaris project list/new/rm`；`polaris chat` 新会话归属按 `--project` > 当前目录匹配（最内层 `root_path` 胜出，归档项目不参与）> 默认项目；上次会话只在属于同一项目时续接（归属以库内为准，否则 `project_id` 被静默忽略）。匹配只在 CLI 做（依赖调用方 cwd）。
- 原「工具文件访问边界本次不改」被决策五推翻；VFS 每任务沙箱根仍未改动。

## 决策五：项目工作目录 = 该项目会话的工具文件访问根（会话级，追加式）

绑定 `root_path` 即授予该项目会话的内置文件/命令工具访问该目录。与 `trusted` 无关：`trusted` 管 AGENTS.md 是否当指令，访问根管工具能否读写。

- 机制：agent 每轮解析项目（`refreshWorkspaceContext`，与上下文装载同窗口），在 `withTaskScopeCtx` 以 `protocol.CtxProjectRootKey` 注入执行 ctx；`guard.ScopedPaths(ctx, allowedPaths)` 把项目目录置首位追加到进程级白名单（bash/run_command 以首项为默认工作目录）；`guard.SearchRoots` 让 grep/glob 未指定路径时只搜项目目录；沙箱 `AllowedPaths` 同步带上（bwrap/Seatbelt 绑定）。走 ctx 是因内置工具是进程级共享闭包、签名不含会话，`protocol.Ctx*Key` 是既有通道；与「不用 ctx 隐式携带 project_id 进 `EnsureSession`」不矛盾（后者是持久化写入，此处是单次只读作用域，键强类型）。
- 收窄不放宽：只追加不替换白名单；默认项目 / 无目录项目行为不变；todo 按会话落库 `session_todos`（`043_session_todos.sql`，ADR-0104），不往用户仓库写文件。
- 三道校验：(1) 写入时 `normalizeProjectRoot` → `guard.CheckScopeRoot`；(2) agent 侧存库规范路径若现解析到别处（被换软链）则不授予；(3) 工具侧 `ScopedPaths` 再跑 `CheckScopeRoot`，不满足视为无项目根（规则收紧后存量数据不放大访问面）。写入类工具仍走 `CheckWritablePath` 黑名单与软链真实路径复核。
- 子 Agent 经决策三的继承获得项目根与项目指令。

## 后果

- 正向：会话按项目组织；工作区上下文协议有真实项目目录输入；信任授予下沉到「项目」单位。
- 负向：上线前直接改 `013_chat.sql`，开发库须删除重建；项目会话工具可访问面比默认项目多一个目录（有意的能力扩张，边界见决策五三道校验）。
- 备份：导出先 `projects` 后 `chat_sessions`（外键序，`sysadmin/budget.go`），会话带 `project_id`，不导出 `trusted`；恢复时引用不存在项目的会话落默认项目而非丢弃。

## 已驳方案 / 反例守护

| 方案 | 驳回理由 |
|---|---|
| 项目 = 纯分组标签 | 不解决工作区上下文锚点与信任下沉 |
| 项目必须绑定目录 | 纯聊天场景被迫选目录；`root_path` 允许为空 |
| ctx 隐式携带 project_id 进 `EnsureSession` | 隐式耦合（HE-3），改显式 `EnsureSessionInProject` |
| 全局记忆 + 检索后过滤 / 仅在检索入口加过滤即宣称隔离 | 泄漏面在召回与重排之间，七路漏一路测试不红；须写入打标 + 逐路过滤 + 可实跑泄漏用例 |
| channel/无头会话建专属分支 | 一律默认项目 |
| 放开非本地客户端写项目 | 见决策二 |

## 重新评估触发条件

1. 七路召回无法逐路证明隔离 → 改为按项目分库而非过滤。
2. 出现「多个项目共享同一目录」的真实需求（当前 `root_path` 允许重复，不去重合并）。
3. 需要把非本地客户端（channel）纳入项目写入面。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-09-21 | 初稿；决策三原规范（Episodic / Semantic / Graph 默认隔离）因写入侧无项目标记、检索三路全局，当日未实施，后修订为收窄口径（情景记忆隔离，语义/反思/画像/Core/知识库全局）并落地 P1~P6 + [L-18] 门控；新增决策五并推翻决策四「工具边界不改」；决策二补位置约束与信任不随备份；决策一补归档拒收；CLI 项目匹配与备份落地 |
| 2026-09-29 | 事实订正：todo 改按会话落库（ADR-0104），月度预算以 `llm_calls` 为唯一来源 |
| 2026-10-12 | 压缩；P5 召回适配器随 ADR-0105 决策十删除旧 Assembler 旁路 |

## 引用代码

`internal/protocol/schema/013_chat.sql`、`internal/store/repo/{repo_project,repo_chat}.go`、`internal/protocol/repo/repo_project.go`、`internal/gateway/server/chat/projects.go`、`internal/agent/context/{workspace_context,memory_project_scope}.go`、`cmd/polaris/{boot_agent,adapters_recall,cli_project}.go`、`internal/tool/builtin/guard/guard.go`、`internal/memory/retrieval/project_scope.go`、`internal/memory/store/episodic_mem.go`、`internal/agent/agent_workspace_project.go`、`internal/gateway/server/sysadmin/budget.go`、`tools/memory_isolation_check.go`；ADR-0088 决策三（信任模型）、ADR-0096 决策一（唯一业务通道）
