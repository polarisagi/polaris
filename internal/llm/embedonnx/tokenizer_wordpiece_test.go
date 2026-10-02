package embedonnx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type testDataFixture struct {
	BGE []struct {
		Text         string    `json:"text"`
		TokenIDs     []int64   `json:"token_ids"`
		Embedding512 []float32 `json:"embedding_512"`
	} `json:"bge"`
	Gemma []struct {
		Text         string    `json:"text"`
		TokenIDs     []int64   `json:"token_ids"`
		Embedding512 []float32 `json:"embedding_512"`
	} `json:"gemma"`
}

func TestWordPieceTokenizerExactMatch(t *testing.T) {
	vocabPath := filepath.Join("testdata", "vocab.txt")
	if _, err := os.Stat(vocabPath); os.IsNotExist(err) {
		t.Skip("testdata/vocab.txt not found")
	}

	tok, err := NewWordPieceTokenizer(vocabPath)
	if err != nil {
		t.Fatalf("failed to create WordPieceTokenizer: %v", err)
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

	if len(td.BGE) == 0 {
		t.Fatal("no BGE test samples found in fixture")
	}

	for i, item := range td.BGE {
		inputIDs, attnMask, tokenTypeIDs := tok.Encode(item.Text, 512)
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
			if tokenTypeIDs[j] != 0 {
				t.Fatalf("sample %d %q tokenTypeIDs[%d] != 0", i, item.Text, j)
			}
		}
	}
}

func TestWordPieceTokenizerTruncation(t *testing.T) {
	vocabPath := filepath.Join("testdata", "vocab.txt")
	if _, err := os.Stat(vocabPath); os.IsNotExist(err) {
		t.Skip("testdata/vocab.txt not found")
	}

	tok, err := NewWordPieceTokenizer(vocabPath)
	if err != nil {
		t.Fatalf("failed to create tokenizer: %v", err)
	}

	longText := "这是一个很长的测试文本，包含了许多重复的字词。" +
		"用于测试当 token 序列超过最大长度上限时是否能被正确截断并保留最后的特殊分隔符。" +
		"重复文字 重复文字 重复文字 重复文字 重复文字 重复文字 重复文字 重复文字 重复文字 重复文字 重复文字 重复文字"

	maxLen := 16
	ids, mask, types := tok.Encode(longText, maxLen)
	if len(ids) != maxLen {
		t.Fatalf("expected len %d, got %d", maxLen, len(ids))
	}
	if len(mask) != maxLen || len(types) != maxLen {
		t.Fatalf("mask and type lengths do not match maxLen")
	}
	if ids[0] != tok.clsID {
		t.Fatalf("expected first token to be [CLS], got %d", ids[0])
	}
	if ids[maxLen-1] != tok.sepID {
		t.Fatalf("expected last token to be [SEP], got %d", ids[maxLen-1])
	}
}
