package embedonnx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func findGemmaTokenizerPath() string {
	candidates := []string{
		filepath.Join("testdata", "tokenizer.model"),
		filepath.Join("..", "..", "..", "local_playground", "upgrade", "local-inference-embedder", "spike", "models", "gemma", "tokenizer.model"),
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.Size() > 0 {
			return c
		}
	}
	return ""
}

func TestSentencePieceTokenizerExactMatch(t *testing.T) {
	tokPath := findGemmaTokenizerPath()
	if tokPath == "" {
		t.Skip("tokenizer.model not found; download to testdata/tokenizer.model to run")
	}

	tok, err := NewSentencePieceTokenizer(tokPath)
	if err != nil {
		t.Fatalf("failed to create SentencePieceTokenizer: %v", err)
	}

	samplesPath := filepath.Join("testdata", "test_samples.json")
	if _, err := os.Stat(samplesPath); os.IsNotExist(err) {
		t.Skip("testdata/test_samples.json not found")
	}

	dataBytes, err := os.ReadFile(samplesPath)
	if err != nil {
		t.Fatalf("failed to read test samples: %v", err)
	}

	var td testDataFixture
	if err := json.Unmarshal(dataBytes, &td); err != nil {
		t.Fatalf("failed to parse test samples: %v", err)
	}

	if len(td.Gemma) == 0 {
		t.Fatal("no Gemma test samples found in fixture")
	}

	for i, item := range td.Gemma {
		inputIDs, attnMask := tok.Encode(item.Text, 512)
		if len(inputIDs) != len(item.TokenIDs) {
			t.Fatalf("sample %d %q length mismatch: got %d, want %d", i, item.Text, len(inputIDs), len(item.TokenIDs))
		}
		for j := range inputIDs {
			if inputIDs[j] != item.TokenIDs[j] {
				t.Fatalf("sample %d %q token %d mismatch: got %d, want %d", i, item.Text, j, inputIDs[j], item.TokenIDs[j])
			}
			if attnMask[j] != 1 {
				t.Fatalf("sample %d %q attnMask[%d] != 1", i, item.Text, j)
			}
		}
	}
}

func TestSentencePieceTokenizerTruncation(t *testing.T) {
	tokPath := findGemmaTokenizerPath()
	if tokPath == "" {
		t.Skip("tokenizer.model not found; download to testdata/tokenizer.model to run")
	}

	tok, err := NewSentencePieceTokenizer(tokPath)
	if err != nil {
		t.Fatalf("failed to create SentencePieceTokenizer: %v", err)
	}

	longText := "这是一个长文本测试句子，用于验证 SentencePiece 分词器在文本超出最大长度时是否能够正常截断并保留最后的 EOS 标记。" +
		"重复文字 重复文字 重复文字 重复文字 重复文字 重复文字 重复文字 重复文字 重复文字 重复文字"

	maxLen := 12
	ids, mask := tok.Encode(longText, maxLen)
	if len(ids) != maxLen {
		t.Fatalf("expected len %d, got %d", maxLen, len(ids))
	}
	if len(mask) != maxLen {
		t.Fatalf("mask length does not match maxLen")
	}
	if ids[0] != gemmaBOSID {
		t.Fatalf("expected first token to be BOS (2), got %d", ids[0])
	}
	if ids[maxLen-1] != gemmaEOSID {
		t.Fatalf("expected last token to be EOS (1), got %d", ids[maxLen-1])
	}
}
