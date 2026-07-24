package benchmark

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type TerminalBenchAdapter struct{}

func (a *TerminalBenchAdapter) Name() string {
	return "terminal-bench"
}

func (a *TerminalBenchAdapter) Load(ctx context.Context, datasetPath string) ([]protocol.EvalCase, error) {
	// TODO-free: 待 Terminal-Bench 数据格式确认后再实现，当前仅登记不实现，避免臆测格式。
	return nil, apperr.Wrap(apperr.CodeUnimplemented, "TerminalBenchAdapter: not implemented yet", nil)
}
