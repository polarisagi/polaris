package adapter

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeTrainingAdapter struct {
	mu      sync.Mutex
	batches [][]TrainingSample
}

func (f *fakeTrainingAdapter) Train(ctx context.Context, samples []TrainingSample) (*TrainingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	batch := make([]TrainingSample, len(samples))
	copy(batch, samples)
	f.batches = append(f.batches, batch)
	return &TrainingResult{JobID: "job"}, nil
}

func (f *fakeTrainingAdapter) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

func TestTrainingSampleCollector_TriggersAtBatchSize(t *testing.T) {
	fake := &fakeTrainingAdapter{}
	c := NewTrainingSampleCollector("test", fake, 3)

	c.Add(TrainingSample{Prompt: "p1", Completion: "c1"})
	c.Add(TrainingSample{Prompt: "p2", Completion: "c2"})
	if fake.batchCount() != 0 {
		t.Fatal("expected no training trigger before batch size reached")
	}
	if got := c.Pending(); got != 2 {
		t.Errorf("expected 2 pending samples, got %d", got)
	}

	c.Add(TrainingSample{Prompt: "p3", Completion: "c3"})

	deadline := time.Now().Add(2 * time.Second)
	for fake.batchCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.batchCount() != 1 {
		t.Fatalf("expected exactly 1 training batch triggered, got %d", fake.batchCount())
	}
	if got := c.Pending(); got != 0 {
		t.Errorf("expected buffer cleared after trigger, got %d pending", got)
	}
}

func TestTrainingSampleCollector_NilSafe(t *testing.T) {
	var c *TrainingSampleCollector
	c.Add(TrainingSample{Prompt: "p", Completion: "c"}) // 不应 panic
	if got := c.Pending(); got != 0 {
		t.Errorf("expected 0 pending for nil collector, got %d", got)
	}

	c2 := NewTrainingSampleCollector("test", nil, 1)
	c2.Add(TrainingSample{Prompt: "p", Completion: "c"}) // adapter=nil，不应 panic
}

func TestNewTrainingSampleCollector_DefaultBatchSize(t *testing.T) {
	c := NewTrainingSampleCollector("test", &fakeTrainingAdapter{}, 0)
	if c.batchSize != 64 {
		t.Errorf("expected default batch size 64, got %d", c.batchSize)
	}
}
