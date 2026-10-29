package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type mockRoundTripper struct {
	receivedBody []byte
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.receivedBody, _ = io.ReadAll(req.Body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte{})),
	}, nil
}

func TestOTLPHTTPExporter_ExportSpan(t *testing.T) {
	mt := &mockRoundTripper{}
	client := &http.Client{Transport: mt}

	exporter := NewOTLPHTTPExporter(client, "http://dummy")
	span := &Span{
		TraceID:   "test-trace",
		SpanID:    "test-span",
		Kind:      SpanLLMCall,
		Name:      "llm.generate",
		StartTime: time.Now(),
		Attrs:     map[string]any{"model": "gpt-4"},
	}

	err := exporter.ExportSpan(context.Background(), span)
	if err != nil {
		t.Fatalf("ExportSpan failed: %v", err)
	}

	var receivedSpan Span
	if err := json.Unmarshal(mt.receivedBody, &receivedSpan); err != nil {
		t.Fatalf("Failed to parse received span: %v", err)
	}

	if receivedSpan.TraceID != "test-trace" {
		t.Errorf("Expected TraceID 'test-trace', got %s", receivedSpan.TraceID)
	}
	if receivedSpan.Kind != SpanLLMCall {
		t.Errorf("Expected Kind 'gen_ai.llm_call', got %s", receivedSpan.Kind)
	}
	val, ok := receivedSpan.Attrs["model"]
	if !ok || val != "gpt-4" {
		t.Errorf("Expected attr model=gpt-4, got %v", val)
	}
}

type mockExporter struct {
	exported atomic.Int32
}

func (m *mockExporter) ExportSpan(ctx context.Context, s *Span) error {
	m.exported.Add(1)
	return nil
}

func (m *mockExporter) Shutdown(ctx context.Context) error { return nil }

func TestTracer_NoExporter(t *testing.T) {
	tracer := NewTracer()
	span, _ := tracer.StartSpan(context.Background(), SpanMemoryOp, "test")
	// This should not panic or hang
	tracer.EndSpan(span)
}

func TestTracer_WithExporter(t *testing.T) {
	tracer := NewTracer()
	mockExp := &mockExporter{}
	tracer.RegisterExporter(mockExp)

	span, _ := tracer.StartSpan(context.Background(), SpanMemoryOp, "test")
	tracer.EndSpan(span)

	// Since EndSpan fires a goroutine, we sleep briefly to allow the mock to be called
	time.Sleep(10 * time.Millisecond)

	if m := mockExp.exported.Load(); m != 1 {
		t.Errorf("Expected 1 export, got %d", m)
	}
}
