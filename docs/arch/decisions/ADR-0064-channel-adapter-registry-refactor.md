# ADR-0064: Channel Adapter Registry Refactor & Unified Inbound Dispatch Wiring

## Status
Accepted

## Context
1. **Channel 适配器注册表重构** (A-1)
   `internal/channel/dispatch.go:18 SendReply`、`message.go:12 ExtractMessage`、`manager.go:74 Start` 三处针对 ~17 个平台使用了巨型 `switch channelType` 结构，严重违反了开闭原则 (OCP, R2.1/HE-3)。各平台的适配逻辑散落在各种自由函数和 `Manager` 的方法中，缺乏统一接口抽象。
2. **统一入站分发接线** (A-2)
   入站消息处理 `dispatchChannelMessage` 虽已实现持久化及会话管理，但 Poller 模式入站消息（如 Telegram 长轮询、Discord Gateway 等）的回调链 `adapter -> host.OnMessage -> Manager.onMessage` 在 `cmd/polaris/boot_server.go:99` 被接成了空函数。导致所有非 Webhook 模式的入站消息全部丢失，无法进入分发、持久化和推理环节。

## Decision

### 1. 引入 Channel 适配器接口与注册表 (A-1)
- 引入 `adapter.Adapter` 接口，定义 `Type()`, `Extract()`, `Send()`, `StartPoller()` 方法。
- 引入 `adapter.Host` 接口，扩展现有的 `PollerHost` 并增加 `WecomEnqueue()` 能力。
- 采用进程内全局注册表（`map[string]Adapter`）配合 `init()` 函数自注册模式。
- `Manager` 的 `Start`, `ExtractMessage`, `SendReply` 改为查表委派。
- **平滑迁移策略**：为保证系统稳定，采用逐个平台迁移，使用原 `switch` 作为未查到表项的 fallback。全部平台迁移完成并测试通过后，再移除 `switch` 结构。

### 2. 补全 Poller 入站消息处理器接线 (A-2)
- 为 `Manager` 增加晚绑定 Setter 方法 `SetMessageHandler(h cadapter.MessageHandler)`。由于多线程并发考虑，底层应使用 `atomic.Pointer`。
- 在 `internal/gateway/server/sysadmin/channelsadmin/` 的 `ChannelsAdmin` 中暴露导出的 `DispatchChannelMessage` 方法。
- 在 `boot_server.go` 初始化 `ChannelsAdmin` 后，立即调用 `channelMgr.SetMessageHandler()` 完成接线。

## Consequences
- **解耦与扩展性**：未来新增聊天平台（如微信、Line等）仅需实现 `Adapter` 接口并注册，无需再触碰核心 `Manager` 代码，符合 OCP。
- **安全性与稳定性**：逐步迁移策略最大程度降低了重构引入的回归风险。
- **数据完整性**：修复了 Poller 模式下消息丢失的重大缺陷，确保了所有渠道对话的多轮上下文都被正确落盘和推理。
