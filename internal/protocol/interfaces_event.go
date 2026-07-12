package protocol

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol/pb"
	"github.com/polarisagi/polaris/pkg/types"
)

// EventLogger 定义了将事件安全、串行写入 M2 events 表的契约。
type EventLogger interface {
	AppendEvent(ctx context.Context, ev *pb.Event) error
}

// DecisionLogger 定义了将架构决策写入 M3 decision_log 表的契约。
type DecisionLogger interface {
	AppendDecision(ctx context.Context, entry *types.DecisionLogEntry) error
}

// EventWriter 接口已删除（2026-07-08）：全仓库零实现，唯一消费方
// internal/llm/router*.go 的 eventWriter 字段因注入方法 WithEventWriter
// 早前被判定死代码删除后恒为 nil、代码路径永久不可达，一并清理。
// 详见 local_playground/reports/phase4-hard-dep-and-deadcode-followup-20260708.md。
