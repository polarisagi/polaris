package store

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
)

// StoreFacade store 包对外统一接口（KV 存储 + 变更总线 + 事务发件箱）。
//
// 问题背景：
//
//	当前 store 包对外暴露了三个独立组件：
//	  - SQLiteStore（实现 protocol.Store KV 接口）
//	  - DatabaseWriter / MutationBus（批量顺序写，LeaseChecker 联动）
//	  - OutboxWorker（事务发件箱，保证至少一次投递）
//	上层代码（protocol.Repo 实现层、outbox 订阅方）分别持有这三个具体 struct，
//	任何存储层切换（SQLite → SurrealDB 热路径）都影响多处调用方。
//
// 解决方案：
//   - StoreFacade 在 protocol.Store（KV 接口）基础上扩展写入和发件箱入口
//   - 上层 Repo 实现只依赖此接口，不直接持有 *SQLiteStore / *DatabaseWriter
//   - StorageRouter（SQLite ↔ SurrealDB 路由）对调用方透明
//
// 注意：
//
//	protocol.Store 已是 KV 层统一接口（Get/Put/Delete/Scan/BatchWrite/Txn/Capabilities/Close）。
//	StoreFacade 在其之上补充变更总线和发件箱访问，供需要这两个能力的上层模块使用。
//
// @consumer: store/repo/*.go，internal/*/provider.go 中的 RepoProvider
// @producer: store.SQLiteStore + store.DatabaseWriter + store.OutboxWorker（由 cli.go/bootstrap 构造）
type StoreFacade interface {
	// 嵌入 protocol.Store：Get / Put / Delete / Scan / BatchWrite / Txn / Capabilities / Close
	protocol.Store

	// Writer 返回批量顺序变更写入器（经 LeaseChecker 检查后写入 WAL）。
	// 供需要高吞吐原子写入的 Repo 层使用（如 EpisodicRepo / EventLog）。
	Writer() MutationWriter

	// Outbox 返回事务发件箱写入入口（写后由 OutboxWorker 后台投递）。
	// 供跨模块异步事件（如 memory consolidation 触发信号）使用。
	Outbox() OutboxDispatcher
}

// MutationWriter store 包对外暴露的批量变更写入接口。
// 实现：store.DatabaseWriter
type MutationWriter interface {
	// Submit 提交单条变更意图（顺序写，经 LeaseChecker 检查后异步 flush）。
	Submit(ctx context.Context, intent *MutationIntent) error
	// SubmitBatch 批量提交变更意图（减少 flush 次数）。
	SubmitBatch(ctx context.Context, intents []*MutationIntent) error
}

// OutboxDispatcher store 包对外暴露的发件箱接口。
// 实现：store.OutboxWorker
type OutboxDispatcher interface {
	// Write 写入一条发件箱记录（事务内原子写，OutboxWorker 后台投递）。
	Write(ctx context.Context, entry protocol.OutboxEntry) error
	// FetchBatch 供 OutboxWorker Run 循环分页拉取待投递记录。
	FetchBatch(ctx context.Context, cursor int64, batchSize int) ([]*OutboxRecord, error)
}
