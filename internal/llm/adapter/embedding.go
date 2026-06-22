package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// OllamaEmbeddingAdapter 对接 Ollama /api/embed，实现 substrate.Embedder。
// FeatureLocalEmbedding 门控；Tier 0 可用（BGE-small 仅需 ~256MB）。
type OllamaEmbeddingAdapter struct {
	model   string
	baseURL string
	client  *http.Client
}

// NewOllamaEmbeddingAdapter 构造本地嵌入适配器。
func NewOllamaEmbeddingAdapter(model string, httpClient *http.Client) *OllamaEmbeddingAdapter {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	return &OllamaEmbeddingAdapter{
		model:   model,
		baseURL: "http://localhost:11434",
		client:  httpClient,
	}
}

// Embed 将文本转换为 float32 向量（实现 substrate.Embedder 接口）。
func (e *OllamaEmbeddingAdapter) Embed(text string) []float32 {
	ctx := context.Background()
	vecs, err := e.EmbedBatch(ctx, []string{text})
	if err != nil || len(vecs) == 0 {
		return nil
	}
	return vecs[0]
}

// EmbedBatch 批量嵌入（减少 HTTP 往返）。
type ollamaEmbedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResp struct {
	Embeddings [][]float32 `json:"embeddings"`
}

func (e *OllamaEmbeddingAdapter) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(ollamaEmbedReq{Model: e.model, Input: texts})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "marshal embed req", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "build embed req", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ollama embed http", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("ollama embed status %d: %s", resp.StatusCode, raw))
	}

	var out ollamaEmbedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "decode embed resp", err)
	}
	return out.Embeddings, nil
}
