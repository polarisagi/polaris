package embedonnx

import (
	"bufio"
	"io"
	"log/slog"
	"os"
	"strings"
	"unicode"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// WordPieceTokenizer 纯 Go 实现的 WordPiece 分词器（适配 bge-small-zh-v1.5）。
// 保留中英大小写，按字拆分 CJK 字符与标点，子词使用 ## 前缀贪婪匹配。
type WordPieceTokenizer struct {
	vocab map[string]int64
	unkID int64
	clsID int64
	sepID int64
	padID int64
}

func isCJK(r rune) bool {
	ranges := [][2]rune{
		{0x4E00, 0x9FFF},
		{0x3400, 0x4DBF},
		{0x20000, 0x2A6DF},
		{0x2A700, 0x2B73F},
		{0x2B740, 0x2B81F},
		{0x2B820, 0x2CEAF},
		{0xF900, 0xFAFF},
		{0x2F800, 0x2FA1F},
	}
	for _, rg := range ranges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

func isPunct(r rune) bool {
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) ||
		(r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	return unicode.IsPunct(r)
}

// NewWordPieceTokenizer 从词表文件构造分词器。
func NewWordPieceTokenizer(vocabPath string) (*WordPieceTokenizer, error) {
	f, err := os.Open(vocabPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "embedonnx: open vocab failed", err)
	}
	defer f.Close()
	return NewWordPieceTokenizerFromReader(f)
}

// NewWordPieceTokenizerFromReader 从 Reader 构造分词器。
func NewWordPieceTokenizerFromReader(r io.Reader) (*WordPieceTokenizer, error) {
	vocab := make(map[string]int64)
	scanner := bufio.NewScanner(r)
	var idx int64
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		vocab[line] = idx
		idx++
	}
	if err := scanner.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "embedonnx: read vocab failed", err)
	}

	return &WordPieceTokenizer{
		vocab: vocab,
		unkID: vocab["[UNK]"],
		clsID: vocab["[CLS]"],
		sepID: vocab["[SEP]"],
		padID: vocab["[PAD]"],
	}, nil
}

func (tok *WordPieceTokenizer) tokenizeWord(word string) []string {
	runes := []rune(word)
	if len(runes) > 100 {
		return []string{"[UNK]"}
	}
	start := 0
	var subTokens []string
	for start < len(runes) {
		end := len(runes)
		var curSub string
		for start < end {
			substr := string(runes[start:end])
			if start > 0 {
				substr = "##" + substr
			}
			if _, ok := tok.vocab[substr]; ok {
				curSub = substr
				break
			}
			end--
		}
		if curSub == "" {
			return []string{"[UNK]"}
		}
		subTokens = append(subTokens, curSub)
		start = end
	}
	return subTokens
}

// Encode 对输入文本分词，返回 input_ids, attention_mask, token_type_ids。
// 超出 maxLen 时截断并保留 [SEP]，记录 DEBUG 日志。
func (tok *WordPieceTokenizer) Encode(text string, maxLen int) ([]int64, []int64, []int64) {
	if maxLen <= 0 {
		maxLen = 512
	}

	var sb strings.Builder
	for _, r := range text {
		if isCJK(r) || isPunct(r) {
			sb.WriteByte(' ')
			sb.WriteRune(r)
			sb.WriteByte(' ')
		} else if unicode.IsSpace(r) {
			sb.WriteByte(' ')
		} else {
			sb.WriteRune(r)
		}
	}

	words := strings.Fields(sb.String())
	subwords := make([]string, 0, len(words))
	for _, word := range words {
		subwords = append(subwords, tok.tokenizeWord(word)...)
	}

	ids := make([]int64, 0, len(subwords)+2)
	ids = append(ids, tok.clsID)
	for _, sw := range subwords {
		if id, ok := tok.vocab[sw]; ok {
			ids = append(ids, id)
		} else {
			ids = append(ids, tok.unkID)
		}
	}
	ids = append(ids, tok.sepID)

	if len(ids) > maxLen {
		slog.Debug("embedonnx: wordpiece input truncated", "original_len", len(ids), "max_len", maxLen)
		ids = ids[:maxLen]
		ids[maxLen-1] = tok.sepID
	}

	seqLen := len(ids)
	attnMask := make([]int64, seqLen)
	tokenTypeIDs := make([]int64, seqLen)
	for i := range attnMask {
		attnMask[i] = 1
		tokenTypeIDs[i] = 0
	}

	return ids, attnMask, tokenTypeIDs
}
