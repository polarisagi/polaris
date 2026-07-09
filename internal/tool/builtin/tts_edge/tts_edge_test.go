package tts_edge

import (
	"context"
	"encoding/json"
	"testing"
)

func TestExecuteEdgeTTS(t *testing.T) {
	fn := MakeExecuteEdgeTTSFn(false, "")
	_, err := fn(context.Background(), []byte("invalid"))
	if err == nil {
		t.Fatal("expected error")
	}

	// Test fallback/mock
	out, err := fn(context.Background(), []byte(`{"text":"test"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]string
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if res["status"] != "success" {
		t.Fatalf("expected success")
	}
	if res["audio_uri"] == "" {
		t.Fatalf("expected audio uri")
	}
}
