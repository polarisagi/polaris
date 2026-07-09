package protocol

import "context"

// OutboxEntry outbox 写入条目。
type OutboxEntry struct {
	TargetEngine   string
	Operation      string
	Scope          string
	Payload        []byte
	IdempotencyKey string
}

// OutboxWriter 最小化 outbox 写入接口。
type OutboxWriter interface {
	Write(ctx context.Context, entry OutboxEntry) error
}
