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

// CSVFanoutEventLogger 由 internal/execute/orchestrator 的 CSV Fan-out 执行器消费，
// 用于记录逐行/批次事件（csv_job_row_*）。与 EventLogger 分开定义是因为该场景使用
// types.Event（非 protobuf pb.Event）契约，字段形状不同，不能直接复用同一接口；
// 按 R1.4 接口定义权属于消费方，收敛至 protocol 包内（原定义在 execute/orchestrator
// 包内属于 producer-side interface 违规，见 gemini-upgrade-prompt.md D6/F4）。
type CSVFanoutEventLogger interface {
	Append(ctx context.Context, ev types.Event) error
}

// EventWriter 接口已删除（2026-07-08）：全仓库零实现，唯一消费方
// internal/llm/router.go*.go 的 eventWriter 字段因注入方法 WithEventWriter
// 早前被判定死代码删除后恒为 nil、代码路径永久不可达，一并清理。
// 详见 local_playground/reports/phase4-hard-dep-and-deadcode-followup-20260708.md。
