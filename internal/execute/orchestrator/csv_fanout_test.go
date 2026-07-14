package orchestrator

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestCSVFanoutJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Write a mock CSV file
	csvData := `id,name,value
1,alice,10
2,bob,20
3,charlie,30`
	err := os.WriteFile("test_fanout.csv", []byte(csvData), 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove("test_fanout.csv")

	job := CSVFanoutJob{
		CSVPath:        "test_fanout.csv",
		OutputCSVPath:  "test_fanout_out.csv",
		IDColumn:       "id",
		Instruction:    "Hello {name}, value is {value}",
		MaxConcurrency: 2,
	}

	b := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 100),
	}
	res, err := RunCSVFanout(ctx, b, job)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	if res.Errors != 3 {
		t.Errorf("expected 3 failed rows, got %d", res.Errors)
	}
}
