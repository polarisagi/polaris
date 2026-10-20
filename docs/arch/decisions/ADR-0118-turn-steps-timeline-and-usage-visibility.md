# ADR-0118: 会话步骤时间线与用量可见性

- **状态**: Accepted
- **日期**: 2026-10-20
- **决策者**: MrLaoLiAI（设计：Claude；实现：Sonnet）
- **相关模块**: M01（价目与记账）/ M13（SSE 协议、状态栏、聊天页、设置页）/ internal/gateway/session / internal/store / internal/llm/pricing
- **修订**: ADR-0116（回合事件新增 `step`、`complete` 携带 `turn_id`/`usage`）；ADR-0117 追记中"workflow/webhook 整体超时未随审批暂停"的局限

## 上下文

用户在对话里看不到"这一轮到底做了什么、花了多少"：思考与工具调用只在运行时短暂闪现，刷新即失；状态栏的费用恒为 0；实测一条约 1.2 万 token 的请求显示为近 2 美元。排查发现费用错在两处：`openai_compat` 适配器把 gpt-4o-mini 的每百万价格填进"每千 token"字段，且按适配器类型而非实际模型取价，DeepSeek 流量被按 gpt-4o-mini 计费；状态栏接口把 `cost_cny` 写死为 0。同时本地令牌每次启动都重新生成，守护进程重启后已打开的页面持续 401 而无任何提示。

## 决策

### 决策一：价目统一为 USD/百万 token，按实际模型取价

费率字段改名 `CostPerMTok*`，换算收口到 `types.CostUSD`（命中部分按命中价）；新增 `internal/llm/pricing` 目录（小写最长前缀匹配），记账先按 `model_id` 查目录、查不到才回退适配器声明费率。DeepSeek 价取自官方定价页高峰价（宁高勿低，预算护栏更安全）。存量 `llm_calls` 一次性重算，重算前备份、写完成标记，可逆。后端所有金额一律 USD。

### 决策二：回合是用量与步骤的统一归属单位——`turn_id`

回合 ID（`turn_<hex>`，TurnHub 分配）经 context 贯穿到 `llm_calls.turn_id` 与 `chat_turn_steps.turn_id`、assistant 消息的 `turn_id/usage`。`complete` 事件携带 `turn_id`、`usage`（本回合聚合）与 `steps` 摘要；`GET /v1/sessions/{id}/usage` 给会话累计与按回合明细；状态栏改读真实用量。

### 决策三：结构化 `step` 事件，只给人看

内核流事件（思考、工具、审批、降级提示、LLM 调用、子智能体）统一转为 `step` 事件（kind/phase/status，同 id 后到覆盖先到），预览有界，思考增量合并推送。**步骤从不写入模型上下文，不增加 token**。旧事件保留以兼容旧客户端。

### 决策四：步骤落库有界，历史回合懒加载

表 `chat_turn_steps`：每回合头部 ≤200 步/160KB + 尾部环 ≤80 步/64KB，中间只计数不存；seq=0 摘要行恒存在，列表读它即可；单个思考步骤截断 8KB；异步批量写，不阻塞回合；随会话级联删除。保留期 `m13_interface.turn_steps.retention_days`（默认 0 永久），6 小时清理任务裁剪。历史回合只在用户展开时才 `GET /v1/sessions/{id}/turns/{turnID}/steps` 加载。

### 决策五：为什么步骤与用量不放浏览器本地存储

1. **多端与重连**：同一会话会在 Web、桌面壳、CLI 间切换，且回合与连接已解耦（ADR-0116，断线重连回放）；本地存储只在写入它的那个浏览器里有，换设备或清站点数据即丢，而用量账是用户核对花费的依据，不能因清缓存而消失。
2. **单一事实源**：用量的权威数据是 `llm_calls`（含后台调用、压缩、画像提炼等用户看不到的回合），前端只能看到自己订阅过的回合；本地累加必然与账本漂移，正是此前"状态栏恒 0"一类错误的来源。
3. **无人值守回合**：cron/workflow/webhook 的回合没有浏览器在场，步骤只能由服务端记录；事后在会话页查看必须来自服务端。
4. **容量与生命周期**：步骤量大，本地存储（约 5MB 配额、同步 API、可被清理）既装不下也无法随会话删除级联、无法按保留期裁剪；服务端有界落库与保留期是可控的。
5. **只把"展示偏好"留在本地**：币种选择（自动/人民币/美元）这类纯展示偏好才存 `localStorage`（读写均 try/catch，失败回落默认）。

### 决策六：展示币种在前端换算

接口只给 USD 与 `fx_cny_per_usd`（`config.UsageDisplayFXCNYPerUSD`，当前 7.1，估算常量）；前端 `fmt` store 按偏好换算，"自动"按界面语言（中文显示人民币、其余美元）。界面以提示文字标注为估算，不声称实时汇率。

### 决策七：本地令牌复用与 401 自愈

令牌文件存在、权限 0600 且格式有效时启动复用；关停只删 PID/端口文件，不删令牌；显式轮换走 `RotateToken`。前端 `net` store 在 fetch 收到 401 时（含日志 SSE 出错后的探测）先尝试重取静态壳以刷新 `polaris_local` Cookie，仍失败再显示"与本地服务的连接已失效，点此重新连接"横幅，不再静默失败。

### 追记：同批落地的相邻修正

- 裁决接口对未知审批 ID 返回 404（此前 200/500 混用），状态码跟随 `apperr` 错误码。
- 工作流整体/单步超时在等待人工审批期间暂停（与 cron 共用 `SuperviseBudget`/`PendingMatcher`），补齐 ADR-0117 追记中的局限；webhook 本就经 `executeAutomation` 暂停。
- 自动化（cron/webhook/routine）新增 `project_id`，运行会话归入该项目，无人值守"以后不再询问"规则按项目记忆。
- `web_search` 默认降级链新增免 key 的 Bing HTML 引擎；同会话 DDG 连续 2 次被限流（202/429）后 5 分钟内跳过 DDG，到期探测一次，成功即复位。

## 后果

- **正向**: 每个回合可展开查看过程与花费；费用回到真实量级（见 M01 §5.3-ter）；重启守护进程不再让页面悄悄失效。
- **负向**: `chat_turn_steps` 增加存储（有界）；旧客户端看不到时间线（旧事件仍可用）；汇率为估算常量，需随行情不定期校正；DeepSeek 高峰/低谷价取高峰，账面略高于实际。
- **反例守护**: 任何把用量累计、步骤历史或"已用多少钱"的权威数据放到浏览器本地存储的提议，须先回答本 ADR 决策五的五点；任何把 `step` 内容并入模型上下文的提议，须先证明其对模型有收益并经用户确认（本 ADR 约束其为零 token 成本）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 步骤与用量存 localStorage/IndexedDB | 见决策五：多端不一致、与账本漂移、无人值守无法记录、不可级联删除与按期裁剪 |
| 步骤无上限全量落库 | 长任务单回合可达数千步，存储与加载代价无界 |
| 后端按用户币种返回金额 | 汇率是展示问题，混入账本会让历史数据随汇率变化而改写 |
| 用模型上下文回放步骤让模型"记得做过什么" | 额外 token 成本，且步骤预览是截断的，不是可靠上下文 |

## 引用代码

- `internal/llm/pricing/pricing.go`、`pkg/types/models_llm.go`（`CostUSD`）、`internal/store/legacy_upgrade.go`（`RepriceLLMCalls`）
- `internal/gateway/session/steps.go`、`steps_turn.go`、`internal/store/repo/repo_chat_turns.go`、`internal/gateway/server/chat/turn_steps.go`
- `internal/gateway/server/sysadmin/session_usage.go`、`internal/gateway/server/server_handlers_hitl.go`（`handleStatus`）
- `internal/runtimeinfo/runtimeinfo.go`（`LoadOrCreateToken`/`RotateToken`）
- `web/src/js/net.js`、`web/src/js/store/{statusBar,steps,chat}.js`
- `internal/tool/builtin/web_search/{bing,breaker}.go`
