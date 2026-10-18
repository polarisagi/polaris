package main

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// harnessAuditAdapter 把 sb.AuditTrail (RecordAudit) 桥接为
// harness_rollback.AuditTrail 和 sleeptime.AuditTrail 所需的接口签名：
// Record(ctx context.Context, actor, action, target string, metadata map[string]any) error。
type harnessAuditAdapter struct {
	at interface {
		RecordAudit(ctx context.Context, toolName string, payload []byte) error
	}
}

func (a *harnessAuditAdapter) Record(ctx context.Context, actor, action, target string, metadata map[string]any) error {
	if a == nil || a.at == nil {
		return nil
	}
	m := map[string]any{
		"actor":    actor,
		"target":   target,
		"metadata": metadata,
	}
	payload, err := json.Marshal(m)
	if err != nil {
		payload = []byte("{}")
	}
	return a.at.RecordAudit(ctx, action, payload)
}

// routerInferCallerAdapter 桥接 *llm.InferenceRouter 至 sleeptime.RouterInferCaller 接口。
type routerInferCallerAdapter struct {
	router *llm.InferenceRouter
}

func (a *routerInferCallerAdapter) Infer(ctx context.Context, req types.InferRequest, opts types.InferOptions) (types.InferResponse, error) {
	if a == nil || a.router == nil {
		return types.InferResponse{}, apperr.New(apperr.CodeInternal, "routerInferCallerAdapter: nil router")
	}
	inferOpts := []types.InferOption{
		types.WithPurpose(types.PurposeBackgroundLLMInfer),
		types.WithThinkingMode(types.ThinkingDisabled),
	}
	if opts.ModelPool != "" {
		inferOpts = append(inferOpts, types.WithModelPool(string(opts.ModelPool)))
	}
	if opts.ThinkingMode != "" {
		inferOpts = append(inferOpts, types.WithThinkingMode(opts.ThinkingMode))
	}
	if opts.Purpose != "" {
		inferOpts = append(inferOpts, types.WithPurpose(opts.Purpose))
	}
	//llmopts:exempt 转发上游 InferOptions，默认 purpose 与 thinking 已在 inferOpts 中声明
	resp, err := a.router.Infer(ctx, req.Messages, inferOpts...)
	if err != nil {
		return types.InferResponse{}, err
	}
	return types.InferResponse{
		Content: resp.Content,
	}, nil
}

// harnessEvaluatorAdapter 桥接 *harness.RunnerImpl 至 sleeptime.Evaluator 接口。
type harnessEvaluatorAdapter struct {
	runner *harness.RunnerImpl
}

func (a *harnessEvaluatorAdapter) Evaluate(ctx context.Context, overlayFiles map[string][]byte) (heldInScore, heldOutScore float64, err error) {
	if a == nil || a.runner == nil {
		return 0.5, 0.5, nil
	}
	trainRep, err := a.runner.RunSuite(ctx, "training", "")
	if err != nil {
		return 0, 0, err
	}
	valRep, err := a.runner.RunSuite(ctx, "validation", "")
	if err != nil {
		return 0, 0, err
	}
	if trainRep != nil && trainRep.TotalCases > 0 {
		heldInScore = float64(trainRep.PassCount) / float64(trainRep.TotalCases)
	}
	if valRep != nil && valRep.TotalCases > 0 {
		heldOutScore = float64(valRep.PassCount) / float64(valRep.TotalCases)
	}
	return heldInScore, heldOutScore, nil
}
