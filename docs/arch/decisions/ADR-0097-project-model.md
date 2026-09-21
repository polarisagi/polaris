# ADR-0097: 项目（Project）模型——会话的运行上下文容器，绑定工作目录，记忆项目隔离

- **状态**: Accepted（决策一~五均已落地；决策三按 2026-09-21 修订口径实施，门控 `make memory-isolation-check`）
- **日期**: 2026-09-21 | **模块**: `internal/protocol/schema/013_chat.sql`, `internal/protocol/repo/`, `internal/store/repo/`, `internal/gateway/server/chat/`, `internal/gateway/session/`, `internal/agent/context/`, `internal/tool/builtin/guard/`, `internal/gateway/server/sysadmin/budget.go`, `cmd/polaris/boot_agent.go`, `cmd/polaris/cli_project.go`, `web/`

## 背景

`chat_sessions` 是扁平表，Web UI 只有一个全局会话列表（`web/src/js/store/sessions.js`），且混杂 channel 来源会话。三条既有事实决定了项目模型的形态：

1. **工作区上下文协议缺锚点**。`internal/agent/context/workspace_context.go`（ADR-0088 决策三）按"工作区根"装载 AGENTS.md / CLAUDE.md，信任由 `agent.trusted_workspace_roots` 全局路径列表授予；但 `workspaceRoot` 在 `boot_agent.go` 里取的是 `VFSWorkspace.GetRootDir()`——每任务沙箱根，不是用户的项目目录。协议实现了，却没有"用户的项目目录"这个输入。
2. **桌面/CLI 形态按目录使用**（ADR-0096）。CLI 天然以当前目录为上下文，桌面版要选文件夹；会话若不能按目录归属，两端无法对齐。
3. **记忆检索目前全局**。`memory/retrieval` 的 KV 前缀扫描（`episodic:` / `chunk:`）、Tier1 SurrealDB FTS、图谱三路均无项目维度。

## 决策一：项目 = 会话集合 + 可选工作目录 + 项目指令 + 信任开关

**项目是会话的运行上下文容器，不是侧边栏分组标签。**

- 表 `projects`（DDL 在 `013_chat.sql`，与 `chat_sessions` 同文件）：`id` / `name` / `root_path`（空串 = 无目录项目，纯聊天）/ `instructions`（项目指令）/ `trusted`（用户对 `root_path` 内上下文文件的显式信任）/ `archived` / 时间戳。
- `chat_sessions.project_id NOT NULL DEFAULT 'default'`。**恒有默认项目**（`id='default'`，建表时种入，不可删除、不可归档）；存量会话、channel 会话、Cron/Workflow/Webhook 的无头会话一律落默认项目，**不设特殊分支**。
- 会话归属在**创建时**确定（`EnsureSessionInProject`，`INSERT OR IGNORE`），聊天请求对已存在会话携带不同 `project_id` 时**以库内为准**。改归属只走显式 API（`PUT /v1/sessions/{id}/project`）。
- 会话创建时该会话的 Agent 才被装配（`AgentPool` 工厂以 sessionID 为键），故 Agent 装配点可由 sessionID 反查项目，无需改动 `buildAgent` 之外的接线。
- 删除项目 = 其会话迁回默认项目，**不级联删除会话**。
- 归档项目**不接收新会话**（`CreateSession` 以 `INSERT ... SELECT ... WHERE archived = 0` 原子判定），其中既有会话照常可续聊——归档是"收起"，不是"冻结"。

## 决策二：`root_path` 的规范化与信任——信任授予是显式的、写入面是收窄的

沿用 ADR-0088 决策三的信任模型，**不放宽**：

- **`trusted` 默认 false**。false 时项目目录内的 AGENTS.md / CLAUDE.md 走 TaintHigh 围栏（"参考资料不是指令"）；true 时才进 ZoneImmutable。理由不变：clone 下来的仓库内容是攻击者可控的。
- **信任判定用规范路径**：写入时对 `root_path` 做 `filepath.Abs` + `filepath.EvalSymlinks`，要求存在且为目录；存库的是规范路径。否则"信任 `/a/proj`、`/a/proj` 是指向别处的软链"即可绕过路径前缀判定。
- **写入面收窄**：项目的创建/修改/删除/改会话归属仅限 `ClientType.IsLocalTrusted()` 或 admin（与 `server_handlers_hitl.go` 中断接口同一口径）。channel 客户端、Webhook 调用方无权创建项目、设置 `trusted` 或改写 `instructions`。
- **`instructions` 是用户自撰内容**，走可信通道（等同 SOUL/偏好级别）；因其写入面已收窄为本地可信客户端，且限长 8 KiB。ADR 明示：若将来放开非本地客户端的写入，**必须先补 SystemPromptGuard 校验与审计**（`server_routes.go` 提示词 Layer1 写入接口注释里的同一三项缺口），不得直接放开。
- 全局 `trusted_workspace_roots` 保留，与项目 `trusted` 取**或**——两者都是用户显式授予。
- **`root_path` 的位置约束**（2026-09-21 追记，随决策五引入）：不得是文件系统根、用户主目录，也不得位于敏感目录内或是敏感目录的祖先（`guard.CheckScopeRoot`，与写入类工具的黑名单同一清单，含各项的真实路径——macOS 上 `/etc` 规范化后是 `/private/etc`）。理由见决策五。
- **信任不随备份迁移**：备份导出不含 `trusted`，恢复一律落 false（`RestoreProject`）。一份外来备份文件不得让任意目录的上下文文件进入可信区。

## 决策三：记忆按项目隔离，另设用户全局层（规范；实施状态：未实施）

**规范**：Episodic / Semantic / Graph 记忆项目间默认隔离；用户全局层（偏好、Persona、Core Memory）跨项目共享。跨项目串记忆会同时破坏用户预期与 HE-2 的信任边界（A 项目里读到 B 项目工作目录的内容）。

**实施状态——本次未落地，原因与门槛**：

- 检索是**全局前缀扫描 + 全局 FTS + 全局图**，写入侧没有项目标记。只在检索入口加 `project_id` 过滤等于"话说了没有"——七路召回里漏一路即泄漏，而漏的那一路在测试里不会红。
- 落地前置条件（缺一不可）：（a）写入侧为 episodic / semantic 条目打项目标记（含 Tier1 SurrealDB 侧）；（b）检索七路各自带过滤；（c）**可实跑的跨项目泄漏用例**：项目 A 写入唯一标记串，项目 B 以七路各自的查询都不得命中，并在 `tools/lint-selftest.txt` 登记负向用例（注入一路不过滤 → 用例报红）。
- 在此之前，**项目不对记忆做任何隔离承诺**：UI 与 API 文档不得暗示隔离已存在。
- 2026-09-21 复核（维持未实施）：前置条件（a）对 episodic 可简化——episodic 事件的 `TaskID` 即 `memoryPartitionKey`（会话 ID 或 swarm 命名空间），经 `chat_sessions.project_id` 即可反查项目，无需新增写入侧标记，且会话改归属时其记忆自然随行。但 Semantic 实体、Reflection、Durative 簇没有会话维度，且"哪些属项目、哪些属用户全局层"（如"用户偏好中文" vs "本项目用 Postgres"）是**产品判定**，不是实现细节，须先定再做；七路中任一路按错误口径过滤都会同时造成泄漏或召回缺失。

### 决策三修订（2026-09-21 追记，规范性变更，先改文档后改代码）：情景记忆按项目隔离，其余为用户全局层

原规范"Episodic / Semantic / Graph 项目间默认隔离"收窄为下表。原结论保留于上文可见。

| 记忆 | 归属 | 理由 |
|---|---|---|
| Episodic 事件（对话/执行轨迹） | **项目** | 项目细节（"上次那个 bug"、仓库里的文件内容、工具输出）都在这里；现状下感知/规划阶段以 `EpisodicQuery{Semantic}` **不带会话过滤**全局子串检索，A 项目的对话片段会直接进入 B 项目的 Prompt |
| Durative 持续簇 | **项目**（随 Episodic） | 簇是 Episodic 事件的 LLM 摘要，内容与来源同域 |
| Episodic 图节点（Spreading Activation） | **项目**（随 Episodic） | 节点即 `episodic:<id>`，回读的是事件原文 |
| Semantic 实体/关系 | 全局 | `semantic_entities UNIQUE(entity_type, name)` 是世界模型——同名实体全局一个节点；按项目拆分需重做信念修正、级联失效与图遍历，且"Go 1.26""用户偏好中文"这类知识本就应跨项目 |
| Reflection | 全局 | 策略级经验（"这类任务先跑测试"），价值在迁移 |
| 用户画像 / Core Memory / Persona | 全局 | 用户层，原规范即如此 |
| 知识库（RAG chunk） | 全局 | 用户导入的文档，与会话无关 |

跨项目的派生事实（Consolidation 从 A 的事件抽取的实体）因此对 B 可见——这是有意的：它们已脱离对话语境且带 `taint_level` 随行，不绕过任何防线。

**归属判定**：写入时打标 `types.Event.ProjectID`——agent 写入点显式填当前项目；`EpisodicMem.Append` 在未显式填写时以 `protocol.ProjectIDFrom(ctx)` 兜底（agent 执行 ctx 由 `withTaskScopeCtx` 注入）。仍为空 = 默认项目（存量数据全部产生于项目概念之前，本就属于默认项目）。**会话改归属不迁移已形成的记忆**——记忆属于它形成时所在的项目。无法解析项目的 Agent（agent-0、课程学习、红队探针等非会话 Agent）以默认项目为当前项目：fail-closed，看不到任何带项目标记的记忆。

**读取面清单**（每条都必须按当前项目过滤，由门控逐条覆盖）：

| # | 读取面 | 过滤点 |
|---|---|---|
| P1/P2 | 感知/规划上下文 `ListEpisodicEvents` | `EpisodicQuery.ProjectID` → `EpisodicMem.Query` |
| P3/P4 | 感知/规划上下文 `cognitive.FTSSearch`（SurrealDB 共享 FTS） | 命中 ID 经 `MemoryFacade.EpisodicProjectOf` 反查，属他项目的事件剔除；非事件命中（实体/扩展条目）放行 |
| P5 | Assembler（`episodicMemAdapter`） | `AssembleRequest.ProjectID` 显式传入 |
| P6 | `memory_search` 工具 → HybridRetriever 七路 | `SearchScope.ProjectID`；`projectScopedSource` **包装整个 DocumentSource**，四个方法的全部产出统一按 Source 分类过滤——结构上不存在"漏一路"：新增召回路只要经 DocumentSource 输出就必然经过过滤 |

不在清单内的读取方均为后台/系统用途且产出落入全局层（Consolidation → Semantic、Reflexion → Reflection、技能演化），或会话自身范围（2PC 预写查询按 SessionID）。MemoryAgent 耳语只投递给 agent-0（非项目会话，按上条以默认项目为界——耳语扫描 `episodic_events` 物化表不含项目列，故耳语内容仅在默认项目 Agent 可见的前提成立，见"已知限制"）。

**门控**：`TestProjectIsolation*`（`internal/memory/...`、`internal/agent/context`）以项目 A 写入唯一标记串、项目 B 经 P1~P6 各自查询均不得命中；`tools/memory_isolation_check.go` 运行这些用例并接入 `make lint`；`tools/lint-selftest.txt` 登记负向用例（拆掉 `projectScopedSource` 包装 → 门控报红）。

**已知限制**：（1）Tier0 SQL 向量路径中未回填 `event_uuid` 的历史行（`episodic_row:` 源）无法反查项目，按默认项目处理；（2）MemoryAgent 耳语当前只接 agent-0，若将来接入会话 Agent，须先给 `episodic_events` 投影补项目列。

### 决策三补：子 Agent 继承发起方项目

委派子 Agent（`transfer_to_agent` → Blackboard → DefaultTaskWorker → `AcquireHeadless`）的会话 ID 是一次性的 `headless-…`，不在 `chat_sessions`，此前解析不到项目：拿不到项目指令、项目目录访问根（决策五），记忆也落入默认项目。

- 继承链路复用既有的共享记忆命名空间（GD-14-001）：发起方无命名空间时以自身会话 ID 作为子任务 `Namespace`，孙任务沿用同一命名空间——命名空间即根会话 ID。项目解析顺序：本会话 → 命名空间所指会话。不新增字段、不新增跨模块协议。
- 命名空间不是会话 ID 的场景（课程学习 `curriculum_*`、CSV 扇出 job ID）解析不到项目，按默认项目处理。
- 命名空间由 Worker 在 `Run()` 之外的 goroutine 注入，解析读取其原子副本；并在意图到达时补刷一次，保证首个 effect 已带上项目作用域。

## 决策四：客户端契约

- REST：`GET/POST /v1/projects`、`GET/PUT/DELETE /v1/projects/{id}`、`GET /v1/sessions?project_id=`、`PUT /v1/sessions/{id}/project`；聊天请求带可选 `project_id`。
- CLI 与桌面外壳能力对齐（ADR-0096 决策一）：同一组 API，外壳不新增业务 Tauri command。CLI 以"当前目录匹配项目 `root_path`"选项目的语法糖属后续增量，不在本次范围。
  - 2026-09-21 追记：已落地（`cmd/polaris/cli_project.go`）。`polaris project list/new/rm`；`polaris chat` 新会话归属按 `--project` > 当前目录匹配（最内层 `root_path` 胜出，归档项目不参与）> 默认项目。上次会话只在**属于同一项目**时续接——否则在 B 目录里会接着 A 的会话聊，而会话归属以库内为准（决策一），`project_id` 被静默忽略。匹配只在 CLI 做：它依赖调用方进程的 cwd，守护进程拿不到。
- 工具的文件访问边界（沙箱 VFS 根是否切换为项目 `root_path`）**本次不改**：它牵涉 `internal/vfs` 与 `tool/sandbox` 的越权面，须单独走安全评审。本次 `root_path` 只驱动工作区上下文装载与 UI 归属。
  - 2026-09-21 追记：本条被决策五推翻（新事实：进程级 `sandbox.allowed_paths` 已是"把用户项目目录加入工具可访问面"的既有机制，项目只是把它从全局收窄到会话；评审结论见决策五）。VFS 根本身仍未改动。

## 决策五：项目工作目录 = 该项目会话的工具文件访问根（会话级，追加式）

**绑定 `root_path` 即授予该项目会话的内置文件/命令工具访问该目录**（与 Codex/Claude 桌面"选工作目录"同义）。与 `trusted` 是两件事：`trusted` 管"目录里的 AGENTS.md 是否当指令"，访问根管"工具能否读写该目录"——用户完全可能要 Agent 修改一个自己不信任其 AGENTS.md 的 clone 仓库。

- **机制**：agent 每轮解析项目（`refreshWorkspaceContext`，与上下文装载同一窗口），在 `withTaskScopeCtx` 以 `protocol.CtxProjectRootKey` 注入执行 ctx；`guard.ScopedPaths(ctx, allowedPaths)` 把项目目录**置首位追加**到进程级白名单（bash/run_command 以首项为默认工作目录，项目会话里命令在项目目录执行）；`guard.SearchRoots` 让 grep/glob 未指定路径时只搜项目目录。沙箱 `AllowedPaths` 同步带上项目目录（bwrap/Seatbelt 绑定）。
- **为什么走 ctx**：内置工具是进程级共享闭包，调用签名不含会话；按调用传递会话域信息的既有通道就是 `protocol.Ctx*Key`（`CtxTaskIDKey`、`CtxAnomalyFilterKey` 同一注入点）。这与"被驳回的方案"里"用 ctx 隐式携带 project_id 进 `EnsureSession`"不矛盾：那条驳的是持久化写入走隐式通道；此处是单次调用的只读作用域，且键是强类型的。
- **收窄不放宽**：只追加、不替换进程级白名单；默认项目 / 无目录项目行为完全不变；todo 文件位置不随项目变化（不往用户仓库里写 `.polaris_todo.json`）。
- **三道校验**：（1）写入时 `normalizeProjectRoot` → `guard.CheckScopeRoot`（决策二位置约束）。根若是 `~`，读类工具只查白名单不查黑名单，`~/.ssh` 即可读——这是位置约束存在的原因；（2）agent 侧：存库规范路径现在若解析到别处（被换成软链），不授予（与信任撤销同一判据）；（3）工具侧 `ScopedPaths` 再跑一次 `CheckScopeRoot`，不满足即视为无项目根（纵深防御：规则收紧后存量数据不放大访问面）。写入类工具仍走 `CheckWritablePath` 黑名单与软链真实路径复核。
- **不在本决策内**：子 Agent（swarm/委派）会话不在 `chat_sessions`，解析不到项目，拿不到项目根与项目指令；VFS 每任务沙箱根不变。
  - 2026-09-21 追记：子 Agent 部分已由"决策三补"解决（经共享记忆命名空间继承发起方项目，项目根与指令随之生效）。VFS 每任务沙箱根仍不变。

## 后果

- **正向**：会话按项目组织；工作区上下文协议第一次有了真实的用户项目目录输入；信任授予从全局路径列表下沉到用户能理解的"项目"单位。
- **负向**：（1）上线前直接改 `013_chat.sql`，开发库须删除重建（`rm ~/.polarisagi/polaris/data/polaris.db`）；（2）备份导出（`sysadmin/budget.go`）尚不含 `projects` 表与会话归属，恢复后会话全部落默认项目；（3）决策三未实施，记忆仍全局。
  - 2026-09-21 追记：（2）已解决——导出先 `projects` 后 `chat_sessions`（外键顺序），会话带 `project_id`；恢复时引用不存在项目的会话（旧备份、项目未随备份）落默认项目而不是丢弃；`trusted` 不导出。（4）新增：项目会话的工具可访问面比默认项目多一个目录（决策五），这是本 ADR 有意的能力扩张，边界见决策五三道校验。
- **反例守护**：驳回"项目只做侧边栏分组"（价值全在目录/指令/信任）；驳回"channel/无头会话建专属分支"（一律默认项目）；驳回"在检索入口加过滤即宣称记忆隔离"（见决策三门槛）；驳回"放开非本地客户端写项目"（见决策二）。

## 被驳回的方案

| 方案 | 驳回理由 |
|---|---|
| 项目 = 纯分组标签（仅 `project_id` 字符串） | 不解决工作区上下文锚点与信任下沉，等于只换了列表排版 |
| 项目 = 必须绑定目录 | 纯聊天场景（写作、问答）被迫选目录；`root_path` 允许为空 |
| 用 `context.Context` 隐式携带 project_id 进 `EnsureSession` | 隐式耦合（HE-3）；改为显式 `EnsureSessionInProject` |
| 全局记忆 + 检索后过滤 | 泄漏面在召回与重排之间，无法证明无泄漏 |

## 引用代码

- `internal/protocol/schema/013_chat.sql`（`projects` 表、`chat_sessions.project_id`）
- `internal/store/repo/repo_project.go`（实现）、`internal/protocol/repo/repo_project.go`（接口）
- `internal/gateway/server/chat/projects.go`（API 与写入面收窄）
- `internal/agent/context/workspace_context.go`（`ListForProject`）、`cmd/polaris/boot_agent.go`（会话→项目装配）
- `internal/tool/builtin/guard/guard.go`（`CheckScopeRoot` / `ScopedPaths` / `SearchRoots`，决策五）
- `internal/memory/retrieval/project_scope.go`（`projectScopedSource`，读取面 P6）、`internal/agent/context/memory_project_scope.go`（P3/P4）、`internal/memory/store/episodic_mem.go`（`ProjectOf`、`EpisodicQuery.ProjectID`）、`internal/agent/agent_workspace_project.go`（项目解析与命名空间回退）
- `tools/memory_isolation_check.go`（[L-18] 门控）
- ADR-0088 决策三（信任模型）、ADR-0096 决策一（唯一业务通道）

## 重新评估触发条件

1. 决策三实施时：若七路召回无法逐路证明隔离，则改为按项目分库（每项目独立 DB 文件）而非过滤，届时更新本 ADR。
2. 出现"多个项目共享同一目录"的真实需求（当前 `root_path` 允许重复但不做去重合并）。
3. 需要把非本地客户端（channel）纳入项目写入面。

## 修订记录

| 日期 | 变更 |
|---|---|
| 2026-09-21 | 初稿；决策三明示未实施并写明门槛 |
| 2026-09-21 | 决策三修订并实施：情景记忆（含持续簇、图节点）按项目隔离，语义/反思/画像/Core 为用户全局层；读取面 P1~P6 逐条过滤；[L-18] 门控 + 三条负向自测。决策三补：子 Agent 经共享记忆命名空间继承发起方项目 |
| 2026-09-21 | 复核：新增决策五（工具访问根）并推翻决策四"工具边界本次不改"；决策二补位置约束与信任不随备份；决策一补归档项目拒收新会话；决策四 CLI、后果（2）备份落地。决策三维持未实施 |
