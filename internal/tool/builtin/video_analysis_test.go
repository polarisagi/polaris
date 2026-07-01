package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

func TestExecuteVideoAnalysis(t *testing.T) {
	fn := makeExecuteVideoAnalysisFn(false, "")
	_, err := fn(context.Background(), []byte("invalid"))
	if err == nil {
		t.Fatal("expected error")
	}

	// Test fallback/mock
	out, err := fn(context.Background(), []byte(`{"video_uri":"file://test.mp4"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if res["status"] != "extracted" {
		t.Fatalf("expected extracted")
	}
	frames := res["frames"].([]any)
	if len(frames) == 0 {
		t.Fatalf("expected mock frames")
	}
}
