package llm

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestTokenizer(t *testing.T) {
	tk := NewTiktokenTokenizer("gpt-4")
	if tk == nil {
		t.Fatalf("expected tokenizer")
	}

	count := tk.CountTokens("hello world")
	if count <= 0 {
		t.Errorf("expected >0 tokens")
	}

	batch := tk.CountTokensBatch([]string{"hello", "world"})
	if len(batch) != 2 {
		t.Errorf("expected 2 batch counts")
	}

	imgCount := tk.CountImageTokens(1024, 1024, "high")
	if imgCount <= 0 {
		t.Errorf("expected >0 image tokens")
	}

	imgCountLow := tk.CountImageTokens(1024, 1024, "low")
	if imgCountLow != 85 {
		t.Errorf("expected 85 low detail tokens")
	}

	vidCount := tk.CountVideoTokens(10.0, 1.0)
	if vidCount <= 0 {
		t.Errorf("expected >0 video tokens")
	}

	bytesCount := tk.CountImageBytesTokens([]byte("fake image data"), "high")
	if bytesCount <= 0 {
		t.Errorf("expected >0 image bytes tokens")
	}

	req := &types.InferRequest{
		Messages: []types.Message{
			{Role: "user", Content: "Hi there"},
			{
				Role: "user",
				Parts: []any{
					types.ImagePart{Width: 100, Height: 100, Detail: "high"},
					types.VideoPart{},
					map[string]any{"text": "Hello"},
				},
				ReasoningContent: "thinking",
			},
		},
	}
	est := tk.EstimateRequest(req)
	if est <= 0 {
		t.Errorf("expected >0 est")
	}

	tkO200 := NewTiktokenTokenizer("gpt-4o")
	if tkO200.encName != "o200k_base" {
		t.Errorf("expected o200k_base")
	}
}
