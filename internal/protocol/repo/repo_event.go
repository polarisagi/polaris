package repo

import "context"

type EventRow struct {
	Offset  int64
	Topic   string
	Type    string
	Payload string
}

type EventRepository interface {
	ListEventsSince(ctx context.Context, offset int64) ([]EventRow, error)
}
