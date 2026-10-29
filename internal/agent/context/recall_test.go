package agentctx

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// blockingKnowledge 模拟不响应 ctx 的检索下游（search.Embedder 无 ctx，内部固定 30s）。
type blockingKnowledge struct{ release chan struct{} }

func (b *blockingKnowledge) SearchRAG(context.Context, string, int) ([]fsm.KnowledgeResult, error) {
	<-b.release
	return nil, nil
}

// TestBuildPerceiveContext_RecallBudget 复现 2026-09-25 实测：Perceive 因召回下游
// 不遵守 ctx 卡 30s。截止到期必须降级为无召回并继续产出 prompt，而非等待或报错。
func TestBuildPerceiveContext_RecallBudget(t *testing.T) {
	kb := &blockingKnowledge{release: make(chan struct{})}
	defer close(kb.release)
	sCtx := &fsm.StateContext{
		RawIntentTS:       taint.NewTaintedString("你好", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "test"),
		TaskModel:         &fsm.TaskModel{Goal: "打招呼"},
		KnowledgeSearcher: kb,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	msgs, err := BuildPerceiveContext(ctx, newRespondTestMemory(), sCtx, nil)
	if err != nil {
		t.Fatalf("召回超时应降级而非报错: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("构建耗时 %v，未遵守截止时间", elapsed)
	}
	if !strings.Contains(joinContents(msgs), "你好") {
		t.Fatal("降级后的 prompt 仍须携带本轮意图")
	}
}
