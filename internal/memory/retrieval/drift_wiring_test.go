package retrieval

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// fakeAnchorRecorder 记录 sampleDriftAnchor/RecordAnchor 调用参数，供断言。
type fakeAnchorRecorder struct {
	calls []struct {
		taskType, query string
		embedding       []float32
		expected        []string
	}
}

func (f *fakeAnchorRecorder) RecordAnchor(taskType, query string, embedding []float32, expected []string) {
	f.calls = append(f.calls, struct {
		taskType, query string
		embedding       []float32
		expected        []string
	}{taskType, query, embedding, expected})
}

type fakeDriftGate struct {
	downgraded map[string]bool
}

func (f *fakeDriftGate) IsDowngraded(taskType string) bool { return f.downgraded[taskType] }

func TestInjectDriftDetector_And_InjectDriftRegistry_WireFields(t *testing.T) {
	hr := NewHybridRetriever(nil)
	recorder := &fakeAnchorRecorder{}
	gate := &fakeDriftGate{downgraded: map[string]bool{"coding": true}}

	hr.InjectDriftDetector(recorder, 0.5)
	hr.InjectDriftRegistry(gate)

	if hr.driftDetector != recorder {
		t.Error("expected driftDetector field to hold injected recorder")
	}
	if hr.driftSampleRate != 0.5 {
		t.Errorf("expected sample rate 0.5, got %v", hr.driftSampleRate)
	}
	if !hr.driftRegistry.IsDowngraded("coding") {
		t.Error("expected injected gate to report coding as downgraded")
	}
}

func TestSampleDriftAnchor_RecordsTopFiveWhenSampled(t *testing.T) {
	hr := NewHybridRetriever(nil)
	recorder := &fakeAnchorRecorder{}
	hr.InjectDriftDetector(recorder, 1.0) // 采样率 1.0：必然命中

	merged := []types.ScoredFragment{
		{Source: "a", Score: 5}, {Source: "b", Score: 4}, {Source: "c", Score: 3},
		{Source: "d", Score: 2}, {Source: "e", Score: 1}, {Source: "f", Score: 0.5},
	}
	hr.sampleDriftAnchor("coding", "how to fix bug", []float32{1, 2, 3}, merged)

	if len(recorder.calls) != 1 {
		t.Fatalf("expected exactly 1 RecordAnchor call, got %d", len(recorder.calls))
	}
	call := recorder.calls[0]
	if call.taskType != "coding" || call.query != "how to fix bug" {
		t.Errorf("unexpected call args: %+v", call)
	}
	if len(call.expected) != 5 {
		t.Errorf("expected top-5 truncation, got %d entries: %v", len(call.expected), call.expected)
	}
}

func TestSampleDriftAnchor_SkipsWhenGated(t *testing.T) {
	merged := []types.ScoredFragment{{Source: "a", Score: 1}}

	t.Run("no detector injected", func(t *testing.T) {
		hr := NewHybridRetriever(nil)
		hr.sampleDriftAnchor("coding", "q", []float32{1}, merged) // 不应 panic
	})

	t.Run("sample rate zero", func(t *testing.T) {
		hr := NewHybridRetriever(nil)
		recorder := &fakeAnchorRecorder{}
		hr.InjectDriftDetector(recorder, 0)
		hr.sampleDriftAnchor("coding", "q", []float32{1}, merged)
		if len(recorder.calls) != 0 {
			t.Error("sample rate 0 should never record")
		}
	})

	t.Run("empty task type", func(t *testing.T) {
		hr := NewHybridRetriever(nil)
		recorder := &fakeAnchorRecorder{}
		hr.InjectDriftDetector(recorder, 1.0)
		hr.sampleDriftAnchor("", "q", []float32{1}, merged)
		if len(recorder.calls) != 0 {
			t.Error("empty task_type should never record")
		}
	})

	t.Run("nil query vector", func(t *testing.T) {
		hr := NewHybridRetriever(nil)
		recorder := &fakeAnchorRecorder{}
		hr.InjectDriftDetector(recorder, 1.0)
		hr.sampleDriftAnchor("coding", "q", nil, merged)
		if len(recorder.calls) != 0 {
			t.Error("nil query embedding should never record")
		}
	})
}
