package harness

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/eval/util"
	"github.com/polarisagi/polaris/internal/protocol"
)

type mockAgent struct {
	output []byte
	tools  []string
	err    error
}

func (m *mockAgent) Run(ctx context.Context, input []byte) ([]byte, []string, error) {
	return m.output, m.tools, m.err
}

type mockProvider struct {
	protocol.Provider
	resp *types.ProviderResponse
	err  error
}

func (m *mockProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return m.resp, m.err
}

func TestRunner(t *testing.T) {
	c1 := `{"id":"1", "level":1, "expected":{"output":"hello"}}`
	c2 := `{"id":"2", "level":1, "expected":{"tools":["t1"]}}`
	c3 := `{"id":"3", "level":4, "severity":"P0", "falsifiability_score":0.9}`

	ms := &mockSQLiteStore{vals: [][]byte{[]byte(c1), []byte(c2), []byte(c3)}}
	evalStore := NewSQLiteEvalStore(ms, control.NewEngine(nil))

	runner := NewRunner(ms, evalStore, config.DefaultThresholds())

	ch := make(chan types.EvalCompletedPayload, 1)
	runner.SetEvalChannel(ch)

	agent := &mockAgent{output: []byte("hello world"), tools: []string{"t1"}}
	runner.InjectAgent(agent)

	provider := &mockProvider{
		resp: &types.ProviderResponse{Content: `{"relevance":10,"accuracy":10,"safety":10,"completeness":10,"passed":true,"reason":"ok"}`},
	}
	runner.InjectProvider(provider)

	report, err := runner.RunSuite(context.Background(), "training", "cand1")
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalCases != 3 {
		t.Errorf("expected 3 cases, got %d", report.TotalCases)
	}

	select {
	case payload := <-ch:
		if payload.Suite != "training" {
			t.Errorf("expected training suite")
		}
	case <-time.After(time.Second):
		t.Errorf("timeout waiting for payload")
	}

	// Test Cancel
	runner.activeRuns["run_1"] = func() {}
	err = runner.Cancel(context.Background(), "run_1")
	if err != nil {
		t.Fatal(err)
	}
	err = runner.Cancel(context.Background(), "run_2")
	if err == nil {
		t.Errorf("expected error canceling non-existent run")
	}
}

func TestExtractJSON(t *testing.T) {
	s := util.ExtractJSON("```json\n{\"a\":1}\n```")
	if s != `{"a":1}` {
		t.Errorf("expected json object, got %s", s)
	}
}

type mockSQLiteStore struct {
	protocol.Store
	vals [][]byte
}

func (m *mockSQLiteStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	return &mockIterator{values: m.vals}, nil
}
