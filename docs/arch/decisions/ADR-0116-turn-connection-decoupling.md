# ADR-0116: 会话回合与连接解耦（后台运行）

- **状态**: Accepted
- **日期**: 2026-10-20
- **决策者**: MrLaoLiAI（设计：Claude；实现：Sonnet）
- **相关模块**: M13（`internal/gateway/session`、`internal/gateway/server/chat`）/ Web UI（`web/src/js/store/chat.js`、`sse.js`）/ CLI（`cmd/polaris/cli.go`）
- **修订**: 推翻 M13 网关层「客户端断连即中止回合」语义（`orchestrator_fsm.go` 中的 GD-13-002 注释；`AgentPool` 归还时的防御性 Abort 是另一条不变量，保留）

## 上下文

回合 ctx 直接取自 `r.Context()`：浏览器切换会话 / 关页面 / 桌面窗口重开即断开 SSE，`runFSMTurn` 的 `ctx.Done()` 分支随即 `Interrupt(Abort)` 掉 Agent（原意「断连不空跑」）。后果：用户切到别的会话，原会话的模型立刻被杀，已花掉的 token 作废；也无法让多个会话并行。前端同时只有一套单例状态（`window._activeSseClient`），在结构上也不支持。

## 决策

**回合生命周期归 `TurnHub` 持有，HTTP 连接只是订阅者；中止只能是显式操作或进程关停。**

1. `session.TurnHub`（纯内存）按 sessionID 登记回合：`{turnID, 事件缓冲, 订阅者, cancel}`。`HandleAgentStream` 用 `context.WithoutCancel(r.Context())` 派生回合 ctx，`go RunTurn(...)`，当前连接作为第一个订阅者；**断开只退订，不取消回合**。
2. 事件缓冲有界（默认 4096 条 / 4MB）：越界先把较旧部分里相邻同类的 Delta/Reasoning 合并（文本不丢），仍越界再从最旧端丢弃。回合结束后保留 5 分钟供重连回放，之后清理。
3. 协议增量：`event: session {session_id, turn_id}` 在会话确定后立即推送（新会话据此重连）；每帧带 `id: <seq>`；`GET /v1/sessions/{id}/stream?after=<seq>` 回放后接实时（无回合 → 204）；`POST /v1/sessions/{id}/abort` 显式中止；同会话已有运行中回合时新的 POST 返回 `409 {"code":"session_busy"}`；会话列表/详情带 `running`。
4. 终止事件保证：回合若未产出 `complete`/`error`（如被中止），`Turn.Finish` 补一条 `complete{aborted:true}`，每个订阅者都以终止事件收尾。
5. 守护进程关停：`Server.Shutdown → ChatHandler.Shutdown → TurnHub.Shutdown`，取消全部回合并等其落库已产出内容。
6. 前端 `chat` store 改为多会话运行时：`runs[sessionID]` 持有各自的状态机，视图字段是对「当前视图所属 run」的访问器；切换会话只摘视图、不停 SSE；断线只走 `GET …/stream?after=lastSeq` 续传，**绝不重新 POST**（会重复提交一轮）。
7. CLI Ctrl+C 显式调用 abort 端点，否则 CLI 退出后回合会在守护进程继续跑。

## 后果

- **正向**: 切会话 / 刷新 / 关窗口不再杀掉回合；多会话并行（受 `AgentPool` 容量约束，池逻辑未改）；token 不因断连作废。
- **负向**: 回合不再随连接自然结束——忘记中止的失控回合会一直跑到自然结束或预算上限（预算机制见 ADR-0115）；缓冲占内存（每回合上限 4MB，结束 5 分钟后释放）；同会话回合内的「插话」（对运行中会话再 POST）被 409 取代，Web UI 本来就在运行中禁用发送，CLI 为串行 REPL，均不受影响。
- **反例守护**: 任何「连接断开即取消回合」「为省 token 在 `r.Context().Done()` 上 Abort」的提议，须先证明解决了切会话杀模型的问题并带用户确认；前端新增代码禁止在 `SSEClient` 断线重连时重新 POST。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 前端保持单会话，切会话前弹确认 | 用户明确要求会话可后台运行、多会话并行 |
| 把事件直接写 DB 作为回放源 | 每个 token 一次写入违背 HE-6 的写放大约束，且回放只需短期窗口，内存有界缓冲足够 |
| 无界缓冲 | 长回合内存线性增长；已有 bounded-cache 门控的同类精神 |
| 断线后重新 POST 续传 | 会重复提交一轮，烧 token |

## 引用代码

- `internal/gateway/session/turnhub.go`（注册表）、`orchestrator_interactive.go`（`KindSession`）、`orchestrator_fsm.go`（ctx 语义注释）
- `internal/gateway/server/chat/sse.go`、`sse_turn.go`、`sse_sink.go`（协议）
- `web/src/js/sse.js`、`web/src/js/store/chat.js`
- `docs/arch/M13-Interface-Scheduler.md §1.2 / §8.3.1`

## 重新评估触发条件

1. 出现失控后台回合导致可测量的 token 浪费（需要引入空闲超时 / 无订阅者超时自动中止）。
2. 需要跨守护进程重启恢复进行中的回合（当前缓冲纯内存，重启即丢）。
3. 多用户 / 远程部署需要按用户限制并发后台回合数。

## 修订记录

- 2026-10-20：初版。
