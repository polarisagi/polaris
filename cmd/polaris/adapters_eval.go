package main

import (
	"context"
	"fmt"

	"github.com/polarisagi/polaris/internal/agent"
	"github.com/polarisagi/polaris/internal/automation/hitl"
	"github.com/polarisagi/polaris/internal/eval/regression"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── evalAgentAdapter ─────────────────────────────────────────────────────────
//
// 将 agent.Agent 适配为 eval.EvalAgent 接口。
// agent.Agent.Run(ctx) error 与 eval.EvalAgent.Run(ctx, []byte) ([]byte, []string, error) 签名不匹配。
type evalAgentAdapter struct {
	agent *agent.Agent
}

func (a *evalAgentAdapter) Run(ctx context.Context, input []byte) ([]byte, []string, error) {
	a.agent.SetTaskIntent(input)

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.agent.Run(ctx)
	}()

	_ = a.agent.SendIntent(types.TriggerIntentReceived)

	select {
	case err := <-errCh:
		if err != nil {
			return nil, nil, err
		}
		return a.agent.GetExecuteResult(), nil, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err() //nolint:wrapcheck
	}
}

// regressionDetectorAdapter 将 eval/regression.LightweightRegressionDetector 适配为 hitl.RegressionDetector。
type regressionDetectorAdapter struct {
	inner *regression.LightweightRegressionDetector
}

func (a *regressionDetectorAdapter) DetectRegression(ctx context.Context, checkpointType string) (*hitl.RegressionReport, error) {
	r, err := a.inner.DetectRegression(ctx, checkpointType)
	if err != nil {
		return nil, fmt.Errorf("detect regression: %w", err)
	}
	return &hitl.RegressionReport{
		Markdown: r.Markdown,
		Severity: r.Severity,
	}, nil
}
