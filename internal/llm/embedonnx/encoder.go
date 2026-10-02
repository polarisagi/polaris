package embedonnx

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	// ModelVersionGemma 是 EmbeddingGemma-300M 512 维版本的唯一标识。
	ModelVersionGemma = "onnx:embeddinggemma-300m-q8@512"
	// ModelVersionBGE 是 bge-small-zh-v1.5 512 维版本的唯一标识。
	ModelVersionBGE = "onnx:bge-small-zh-v1.5-int8@512"

	// GemmaDocPrefix 是 EmbeddingGemma 模型卡要求的文档前缀。
	// 出处：onnx-community/embeddinggemma-300m-ONNX 模型卡规范：
	// 文档输入必须为 "title: none | text: {text}"。
	GemmaDocPrefix = "title: none | text: "

	// GemmaQueryPrefix 是 EmbeddingGemma 模型卡要求的查询前缀。
	// 出处：onnx-community/embeddinggemma-300m-ONNX 模型卡规范：
	// 查询输入必须为 "task: search result | query: {text}"。
	GemmaQueryPrefix = "task: search result | query: "
)

// EmbedEngine 是进程内 ONNX 向量化引擎实例。
// 实现 search.Embedder (Embed) 及 EmbedBatch、ModelVersion、Dim 接口契约。
type EmbedEngine struct {
	session       *OrtSession
	wordpiece     *WordPieceTokenizer
	sentencepiece *SentencePieceTokenizer
	isGemma       bool
	version       string
}

// NewGemmaEngine 构造 EmbeddingGemma-300M 引擎。
func NewGemmaEngine(session *OrtSession, tok *SentencePieceTokenizer) *EmbedEngine {
	return &EmbedEngine{
		session:       session,
		sentencepiece: tok,
		isGemma:       true,
		version:       ModelVersionGemma,
	}
}

// NewBGEEngine 构造 bge-small-zh-v1.5 引擎。
func NewBGEEngine(session *OrtSession, tok *WordPieceTokenizer) *EmbedEngine {
	return &EmbedEngine{
		session:   session,
		wordpiece: tok,
		isGemma:   false,
		version:   ModelVersionBGE,
	}
}

// Dim 返回向量维度（固定 512）。
func (e *EmbedEngine) Dim() int {
	return 512
}

// ModelVersion 返回模型版本字符串。
func (e *EmbedEngine) ModelVersion() string {
	return e.version
}

// Close 释放引擎底层资源。
func (e *EmbedEngine) Close() {
	if e.session != nil {
		e.session.Close()
	}
}

// Embed 实现 search.Embedder 接口。失败时返回 nil（触发 FTS 降级）。
func (e *EmbedEngine) Embed(ctx context.Context, text string) []float32 {
	vecs, err := e.EmbedBatch(ctx, []string{text})
	if err != nil || len(vecs) == 0 {
		if err != nil {
			slog.Warn("embedonnx: Embed failed", "err", err)
		}
		return nil
	}
	return vecs[0]
}

// EmbedBatch 批量计算向量。
// 对 Gemma 自动添加模型卡文档前缀；批次间通过 Gosched + 1ms 小睡避免挤占 100% CPU。
func (e *EmbedEngine) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}

	results := make([][]float32, len(texts))
	for i, text := range texts {
		select {
		case <-ctx.Done():
			return nil, ctx.Err() //nolint:wrapcheck
		default:
		}

		var inputIDs, attnMask, tokenTypeIDs []int64
		if e.isGemma {
			formatted := GemmaDocPrefix + text
			inputIDs, attnMask = e.sentencepiece.Encode(formatted, 512)
		} else {
			inputIDs, attnMask, tokenTypeIDs = e.wordpiece.Encode(text, 512)
		}

		vec, err := e.session.Run(inputIDs, attnMask, tokenTypeIDs)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "embedonnx: batch inference failed", err)
		}
		results[i] = vec

		// 后台多条批次时主动让出 CPU 调度并短暂休眠 1ms，防止独占核心（ADR-0109 D9）
		if len(texts) > 1 && i < len(texts)-1 {
			runtime.Gosched()
			time.Sleep(1 * time.Millisecond)
		}
	}

	return results, nil
}
