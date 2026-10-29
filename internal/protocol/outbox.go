package protocol

import "context"

// OutboxEntry outbox 写入条目。
type OutboxEntry struct {
	TargetEngine   string
	Operation      string
	Scope          string
	Payload        []byte
	IdempotencyKey string
	// RetryOnDead 同键记录已存在时的语义：非 dead（pending/failed/processing/done）幂等忽略并返回 nil；
	// dead 则替换为新的 pending 记录。用于用户可重复触发的操作（卸载），避免一次失败永久堵死同键。
	// 默认 false 保持原行为（同键冲突返回错误）。
	RetryOnDead bool
}

// OutboxWriter 最小化 outbox 写入接口。
type OutboxWriter interface {
	Write(ctx context.Context, entry OutboxEntry) error
}
