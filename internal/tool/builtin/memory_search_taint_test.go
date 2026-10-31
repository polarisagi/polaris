package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type taintSearchRetriever struct{ results []types.ScoredFragment }

func (r taintSearchRetriever) Search(context.Context, string, types.SearchScope, types.RetrievalConfig) ([]types.ScoredFragment, error) {
	return r.results, nil
}

func TestRegisteredMemorySearchNilRetrieverFailsExecution(t *testing.T) {
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	router := sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 1)
	envelope := sandbox.NewExecEnvelope(dummyPolicyGate{}, router, 1, "linux", nil)
	toolReg := tool.NewInMemoryToolRegistry(envelope, config.DefaultThresholds().M7Tool)
	if err := RegisterMemoryTools(sbx, toolReg, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	result, err := toolReg.ExecuteTool(context.Background(), "memory_search", []byte(`{"query":"fixture"}`), types.TaintNone)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Success {
		t.Fatalf("nil retriever must be recorded as failed execution: %#v", result)
	}
	if !strings.Contains(result.Error, string(apperr.CodeServiceUnavailable)) {
		t.Fatalf("expected standard SERVICE_UNAVAILABLE error, got %q", result.Error)
	}
	if result.TaintLevel < types.TaintHigh {
		t.Fatalf("failed memory search taint=%v want at least High", result.TaintLevel)
	}
}

func TestRegisteredMemorySearchPropagatesFragmentTaintThroughEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		level types.TaintLevel
		want  types.TaintLevel
	}{
		{name: "unknown low result gets high floor", level: types.TaintLow, want: types.TaintHigh},
		{name: "reviewed result stays reviewed", level: types.TaintUserReviewed, want: types.TaintUserReviewed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
			router := sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 1)
			envelope := sandbox.NewExecEnvelope(dummyPolicyGate{}, router, 1, "linux", nil)
			toolReg := tool.NewInMemoryToolRegistry(envelope, config.DefaultThresholds().M7Tool)
			fragments := []types.ScoredFragment{{Content: "fixture", TaintLevel: tc.level}}
			if err := RegisterMemoryTools(sbx, toolReg, nil, nil, taintSearchRetriever{results: fragments}, nil, nil); err != nil {
				t.Fatal(err)
			}
			result, err := toolReg.ExecuteTool(context.Background(), "memory_search", []byte(`{"query":"fixture"}`), types.TaintNone)
			if err != nil {
				t.Fatal(err)
			}
			if result == nil {
				t.Fatal("registered search returned a nil result")
			}
			if result.TaintLevel != tc.want {
				t.Fatalf("registered search result taint=%v want %v; result=%#v", result.TaintLevel, tc.want, result)
			}
		})
	}
}

var _ protocol.HybridRetriever = taintSearchRetriever{}
