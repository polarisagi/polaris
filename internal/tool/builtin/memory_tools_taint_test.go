package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/memory/retrieval"
	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func contextWithMemoryToolTaint(level types.TaintLevel) context.Context {
	return context.WithValue(context.Background(), protocol.CtxTaintLevelKey{}, level)
}

func TestMemoryWriteTaintFloorAndContext(t *testing.T) {
	ctx := contextWithMemoryToolTaint(types.TaintHigh)
	semantic := store.NewSemanticMem(testutil.NewMockStore(), &testutil.MockIntentSubmitter{})
	fn := MakeMemoryWriteFn(retrieval.NewExclusiveWriter(semantic, nil, nil))
	input, err := json.Marshal(memoryWriteArgs{Name: "high-context", Description: "from high context"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fn(ctx, input); err != nil {
		t.Fatal(err)
	}
	entity, err := semantic.GetEntity(context.Background(), "Fact", "high-context")
	if err != nil {
		t.Fatal(err)
	}
	if entity.TaintLevel != types.TaintHigh {
		t.Fatalf("context taint must be retained: got %v, want %v", entity.TaintLevel, types.TaintHigh)
	}
	input, err = json.Marshal(memoryWriteArgs{Name: "high-context", Description: "later low context update"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fn(contextWithMemoryToolTaint(types.TaintLow), input); err != nil {
		t.Fatal(err)
	}
	entity, err = semantic.GetEntity(context.Background(), "Fact", "high-context")
	if err != nil {
		t.Fatal(err)
	}
	if entity.TaintLevel != types.TaintHigh {
		t.Fatalf("memory_write must not downgrade an existing entity: got %v", entity.TaintLevel)
	}

	lowCtx := contextWithMemoryToolTaint(types.TaintLow)
	input, err = json.Marshal(memoryWriteArgs{Name: "floor", Description: "model generated"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fn(lowCtx, input); err != nil {
		t.Fatal(err)
	}
	entity, err = semantic.GetEntity(context.Background(), "Fact", "floor")
	if err != nil {
		t.Fatal(err)
	}
	if entity.TaintLevel != types.TaintHigh {
		t.Fatalf("memory_write must retain High floor: got %v", entity.TaintLevel)
	}
}

func TestMemoryAppendPreservesExistingAndContextTaint(t *testing.T) {
	ctx := contextWithMemoryToolTaint(types.TaintNone)
	semantic := store.NewSemanticMem(testutil.NewMockStore(), &testutil.MockIntentSubmitter{})
	if err := semantic.UpsertFact(ctx, types.Entity{
		Name: "append-high", Type: "Fact", TaintLevel: types.TaintHigh,
		Properties: map[string]any{"description": "existing"},
	}, types.TaintHigh); err != nil {
		t.Fatal(err)
	}
	fn := MakeMemoryAppendFn(semantic)
	input, err := json.Marshal(memoryAppendArgs{EntityType: "Fact", Name: "append-high", PropKey: "note", PropValue: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fn(ctx, input); err != nil {
		t.Fatal(err)
	}
	entity, err := semantic.GetEntity(context.Background(), "Fact", "append-high")
	if err != nil {
		t.Fatal(err)
	}
	if entity.TaintLevel != types.TaintHigh {
		t.Fatalf("append must retain existing taint: got %v", entity.TaintLevel)
	}

	input, err = json.Marshal(memoryAppendArgs{EntityType: "Fact", Name: "append-new", PropKey: "note", PropValue: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fn(ctx, input); err != nil {
		t.Fatal(err)
	}
	entity, err = semantic.GetEntity(context.Background(), "Fact", "append-new")
	if err != nil {
		t.Fatal(err)
	}
	if entity.TaintLevel != types.TaintMedium {
		t.Fatalf("new append must apply Medium floor: got %v", entity.TaintLevel)
	}

	highContext := contextWithMemoryToolTaint(types.TaintHigh)
	input, err = json.Marshal(memoryAppendArgs{EntityType: "Fact", Name: "append-context", PropKey: "note", PropValue: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fn(highContext, input); err != nil {
		t.Fatal(err)
	}
	entity, err = semantic.GetEntity(context.Background(), "Fact", "append-context")
	if err != nil {
		t.Fatal(err)
	}
	if entity.TaintLevel != types.TaintHigh {
		t.Fatalf("append must retain context taint: got %v", entity.TaintLevel)
	}

	input, err = json.Marshal(memoryAppendArgs{EntityType: "Fact", Name: "append-no-context", PropKey: "note", PropValue: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fn(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	entity, err = semantic.GetEntity(context.Background(), "Fact", "append-no-context")
	if err != nil {
		t.Fatal(err)
	}
	if entity.TaintLevel != types.TaintHigh {
		t.Fatalf("append must fail closed to High without context taint: got %v", entity.TaintLevel)
	}

	badContext := context.WithValue(context.Background(), protocol.CtxTaintLevelKey{}, "invalid")
	input, err = json.Marshal(memoryAppendArgs{EntityType: "Fact", Name: "append-bad-context", PropKey: "note", PropValue: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fn(badContext, input); err != nil {
		t.Fatal(err)
	}
	entity, err = semantic.GetEntity(context.Background(), "Fact", "append-bad-context")
	if err != nil {
		t.Fatal(err)
	}
	if entity.TaintLevel != types.TaintHigh {
		t.Fatalf("append must fail closed to High for invalid context taint: got %v", entity.TaintLevel)
	}
}
