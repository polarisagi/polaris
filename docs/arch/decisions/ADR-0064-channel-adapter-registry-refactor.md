# ADR-0064: Channel Adapter Registry Refactor & Unified Inbound Dispatch Wiring

## Status
Accepted

## Context
1. **Channel 适配器注册表重构** (A-1)
   `internal/channel/dispatch.go:18 SendReply`、`message.go:12 ExtractMessage`、`manager.go:74 Start` 三处针对 ~17 个平台使用了巨型 `switch channelType` 结构，严重违反了开闭原则 (OCP, R2.1/HE-3)。各平台的适配逻辑散落在各种自由函数和 `Manager` 的方法中，缺乏统一接口抽象。
2. **统一入站分发接线** (A-2)
   入站消息处理 `dispatchChannelMessage` 虽已实现持久化及会话管理，但 Poller 模式入站消息（如 Telegram 长轮询、Discord Gateway 等）的回调链 `adapter -> host.OnMessage -> Manager.onMessage` 在 `cmd/polaris/boot_server.go:99` 被接成了空函数。导致所有非 Webhook 模式的入站消息全部丢失，无法进入分发、持久化和推理环节。

## Decision

### 1. 引入 Channel 适配器接口与单例查表 (A-1)
- 引入 `adapter.Adapter` 接口，定义 `Type()`, `Extract()`, `Send()`, `StartPoller()` 方法。
- 引入 `adapter.Host` 接口，直接复用现有的 `PollerHost`（wecom 的发送通道改为 `WecomAdapter` 自身持有的 `sync.Map` 字段，不再需要 Host 额外扩展方法）。
- `GetAdapter(channelType) (Adapter, bool)` 收敛为 `adapter.go` 内一处 `switch`，每个平台返回各自的 `sync.OnceValue` 惰性单例（而非每次调用新建实例）。
  - 2026-07-23 复核订正：最初采用"包级全局 `map[string]Adapter` + 各平台 `init()` 自注册"方案，但该方案与本项目 `internal/` 禁止全局可变变量的红线（`Test_inv_NoGlobalVar`）直接冲突；执行过程中一度为绕开该检查退化为无状态 factory-style `switch`（每次 `GetAdapter` 都 `return &WecomAdapter{}, true` 构造全新实例），导致 `WecomAdapter.wecomSends`/`MatrixAdapter.txnCounter` 这类必须跨 `StartPoller`→`Send` 调用持久的实例状态被静默清空——wecom 回复因此完全丢失、matrix 消息可能因事务 ID 重复被服务端去重。复核时改为 `sync.OnceValue` 单例（该模式是 `Test_inv_NoGlobalVar` 的既定豁免类别，语义为"惰性只读单次计算"），同时解决了合规与状态持久两个问题。
- `Manager` 的 `Start`, `ExtractMessage`, `SendReply` 改为查表委派（三处巨型 `switch` 全部消除，只剩 `adapter.go` 内这一处按平台名分派的 `switch`）。
- **平滑迁移策略**：为保证系统稳定，采用逐个平台迁移，使用原 `switch` 作为未查到表项的 fallback。全部平台迁移完成并测试通过后，再移除 `switch` 结构。

### 2. 补全 Poller 入站消息处理器接线 (A-2)
- 为 `Manager` 增加晚绑定 Setter 方法 `SetMessageHandler(h cadapter.MessageHandler)`。由于多线程并发考虑，底层应使用 `atomic.Pointer`。
- 在 `internal/gateway/server/sysadmin/channelsadmin/` 的 `ChannelsAdmin` 中暴露导出的 `DispatchChannelMessage` 方法。
- 在 `boot_server.go` 初始化 `ChannelsAdmin` 后，立即调用 `channelMgr.SetMessageHandler()` 完成接线。

## Consequences
- **解耦与扩展性**：`Manager`/`dispatch.go`/`message.go` 三处巨型 `switch` 已消除，未来新增聊天平台只需新增该平台自己的 `<platform>.go` 文件实现 `Adapter` 接口，并在 `adapter.go` 的 `GetAdapter` 里补一个 `case` 分支（受限于本项目禁全局可变变量的约束，无法做到零改动的完全自注册，但改动范围已从"三处巨型 switch"收敛到"一处按平台名分派的 switch"）。
- **安全性与稳定性**：逐步迁移策略最大程度降低了重构引入的回归风险。
- **数据完整性**：修复了 Poller 模式下消息丢失的重大缺陷，确保了所有渠道对话的多轮上下文都被正确落盘和推理。
