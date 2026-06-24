package analysis

import (
	"github.com/polarisagi/polaris/internal/eval/harness"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

type ShadowDiff struct {
	BaselineOutput  []byte `json:"baseline_output"`
	CandidateOutput []byte `json:"candidate_output"`
	Diverged        bool   `json:"diverged"`
}

type ShadowExecutor struct {
	baseline  harness.EvalAgent
	candidate harness.EvalAgent
	store     protocol.Store
}

func NewShadowExecutor(baseline, candidate harness.EvalAgent, store protocol.Store) *ShadowExecutor {
	return &ShadowExecutor{
		baseline:  baseline,
		candidate: candidate,
		store:     store,
	}
}

func (s *ShadowExecutor) Compare(ctx context.Context, input []byte) (*ShadowDiff, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	type result struct {
		out []byte
		err error
	}

	baseCh := make(chan result, 1)
	candCh := make(chan result, 1)

	concurrent.SafeGo(ctx, "shadow_executor_baseline", func(ctx context.Context) {
		out, _, err := s.baseline.Run(ctx, input)
		baseCh <- result{out, err}
	})
	concurrent.SafeGo(ctx, "shadow_executor_candidate", func(ctx context.Context) {
		out, _, err := s.candidate.Run(ctx, input)
		candCh <- result{out, err}
	})

	var baseRes, candRes result
	var baseOk, candOk bool

	for !baseOk || !candOk {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-baseCh:
			baseRes = r
			baseOk = true
		case r := <-candCh:
			candRes = r
			candOk = true
		}
	}

	if baseRes.err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "baseline error", baseRes.err)
	}
	if candRes.err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "candidate error", candRes.err)
	}

	diff := &ShadowDiff{
		BaselineOutput:  baseRes.out,
		CandidateOutput: candRes.out,
		Diverged:        !bytes.Equal(baseRes.out, candRes.out),
	}

	if s.store != nil {
		runID := fmt.Sprintf("%d", time.Now().UnixNano())
		key := fmt.Sprintf("shadow:%s:diff", runID)
		data, marshalErr := json.Marshal(diff)
		if marshalErr != nil {
			slog.WarnContext(ctx, "shadow_executor: marshal diff failed", "key", key, "error", marshalErr)
			return diff, nil
		}
		if err := s.store.Put(ctx, []byte(key), data); err != nil {
			slog.WarnContext(ctx, "shadow_executor: persist diff failed", "key", key, "error", err)
		}
	}

	return diff, nil
}
