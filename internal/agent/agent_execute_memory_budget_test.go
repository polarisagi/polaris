package agent

import (
	"context"
	"testing"
	"time"

	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// blockingRetriever 模拟不响应 ctx 的下游（search.Embedder 无 ctx，内部固定 30s）。
type blockingRetriever struct{ release chan struct{} }

func (b *blockingRetriever) Query(context.Context, string, types.TaintLevel, string) ([]agentctx.ContextItem, error) {
	<-b.release
	return []agentctx.ContextItem{{Content: "late", Source: "episodic"}}, nil
}

// TestAssembleWithBudget_AbandonsNonCancellableRecall 复现 2026-09-24 实测：召回下游
// 不遵守 ctx 时，每个 LLM 阶段白等 30s。预算到期必须放弃等待并降级。
func TestAssembleWithBudget_AbandonsNonCancellableRecall(t *testing.T) {
	r := &blockingRetriever{release: make(chan struct{})}
	defer close(r.release) // 让后台 goroutine 退出，避免测试间泄漏
	a := &Agent{assembler: agentctx.NewAssembler(r, nil), sCtx: &fsm.StateContext{}}

	start := time.Now()
	_, err := a.assembleWithBudget(context.Background(), agentctx.AssembleRequest{Query: "q"})
	elapsed := time.Since(start)

	if !apperr.IsCode(err, apperr.CodeTimeout) {
		t.Fatalf("预算到期应返回 CodeTimeout，得到 %v", err)
	}
	if elapsed > memoryAssembleBudget+time.Second {
		t.Fatalf("等待 %v，超出预算 %v", elapsed, memoryAssembleBudget)
	}
}

func TestInjectMemoryToMsgs_DegradesOnBudget(t *testing.T) {
	r := &blockingRetriever{release: make(chan struct{})}
	defer close(r.release)
	a := &Agent{
		assembler: agentctx.NewAssembler(r, nil),
		sCtx:      &fsm.StateContext{TaskModel: &fsm.TaskModel{Goal: "g"}},
	}
	in := []types.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}
	out := a.injectMemoryToMsgs(context.Background(), in)
	if len(out) != len(in) {
		t.Fatalf("召回超时应原样返回消息，得到 %d 条", len(out))
	}
}
