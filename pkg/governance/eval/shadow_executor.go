package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

type ShadowDiff struct {
	BaselineOutput  []byte `json:"baseline_output"`
	CandidateOutput []byte `json:"candidate_output"`
	Diverged        bool   `json:"diverged"`
}

type ShadowExecutor struct {
	baseline  EvalAgent
	candidate EvalAgent
	store     protocol.Store
}

func NewShadowExecutor(baseline, candidate EvalAgent, store protocol.Store) *ShadowExecutor {
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

	go func() {
		out, _, err := s.baseline.Run(ctx, input)
		baseCh <- result{out, err}
	}()
	go func() {
		out, _, err := s.candidate.Run(ctx, input)
		candCh <- result{out, err}
	}()

	var baseRes, candRes result
	baseRes = <-baseCh
	candRes = <-candCh

	if baseRes.err != nil {
		return nil, fmt.Errorf("baseline error: %w", baseRes.err)
	}
	if candRes.err != nil {
		return nil, fmt.Errorf("candidate error: %w", candRes.err)
	}

	diff := &ShadowDiff{
		BaselineOutput:  baseRes.out,
		CandidateOutput: candRes.out,
		Diverged:        !bytes.Equal(baseRes.out, candRes.out),
	}

	if s.store != nil {
		runID := fmt.Sprintf("%d", time.Now().UnixNano())
		key := fmt.Sprintf("shadow:%s:diff", runID)
		data, _ := json.Marshal(diff)
		_ = s.store.Put(ctx, []byte(key), data)
	}

	return diff, nil
}
