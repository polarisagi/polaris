package embedonnx

import (
	"log/slog"

	"github.com/eliben/go-sentencepiece"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// SentencePieceTokenizer 封装纯 Go 实现的 SentencePiece 分词器（适配 EmbeddingGemma-300M）。
type SentencePieceTokenizer struct {
	proc *sentencepiece.Processor
}

const (
	gemmaBOSID = 2
	gemmaEOSID = 1
)

// NewSentencePieceTokenizer 从 tokenizer.model 路径加载分词器。
func NewSentencePieceTokenizer(modelPath string) (*SentencePieceTokenizer, error) {
	proc, err := sentencepiece.NewProcessorFromPath(modelPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "embedonnx: load sentencepiece failed", err)
	}
	return &SentencePieceTokenizer{proc: proc}, nil
}

// Encode 对输入文本分词，自动添加 BOS (2) 与 EOS (1)。
// 超出 maxLen 时截断并保留 EOS (1)，记录 DEBUG 日志。
func (tok *SentencePieceTokenizer) Encode(text string, maxLen int) ([]int64, []int64) {
	if maxLen <= 0 {
		maxLen = 512
	}

	tokens := tok.proc.Encode(text)
	ids := make([]int64, 0, len(tokens)+2)
	ids = append(ids, gemmaBOSID)
	for _, t := range tokens {
		ids = append(ids, int64(t.ID))
	}
	ids = append(ids, gemmaEOSID)

	if len(ids) > maxLen {
		slog.Debug("embedonnx: sentencepiece input truncated", "original_len", len(ids), "max_len", maxLen)
		ids = ids[:maxLen]
		ids[maxLen-1] = gemmaEOSID
	}

	attnMask := make([]int64, len(ids))
	for i := range attnMask {
		attnMask[i] = 1
	}

	return ids, attnMask
}
