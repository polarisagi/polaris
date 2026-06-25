package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

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

// ─── OpenAICompatibleEmbeddingAdapter ────────────────────────────────────────
// 对接任何支持 POST /v1/embeddings 的 Provider（OpenAI 协议）。
// 实现 search.Embedder 接口（Embed 单条）和 EmbedBatch（批量，减少 HTTP 往返）。
// 相比 OllamaEmbeddingAdapter：不绑定本地 Ollama 进程，可用 DeepSeek / OpenAI 等云端 API。
type OpenAICompatibleEmbeddingAdapter struct {
	model   string
	apiKey  []byte
	baseURL string
	client  *http.Client
}

// NewOpenAICompatibleEmbeddingAdapter 构造 OpenAI 兼容 Embedding 适配器。
// apiKey 为空时由调用方从环境变量中读取。
func NewOpenAICompatibleEmbeddingAdapter(baseURL, model string, apiKey []byte, hc *http.Client) *OpenAICompatibleEmbeddingAdapter {
	if hc == nil {
		hc = defaultHTTPClient()
	}
	return &OpenAICompatibleEmbeddingAdapter{
		model:   model,
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  hc,
	}
}

// Embed 实现 search.Embedder 接口（单条文本）。
func (e *OpenAICompatibleEmbeddingAdapter) Embed(text string) []float32 {
	vecs, err := e.EmbedBatch(context.Background(), []string{text})
	if err != nil || len(vecs) == 0 {
		return nil
	}
	return vecs[0]
}

// openAIEmbedReq OpenAI /v1/embeddings 请求体。
type openAIEmbedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// openAIEmbedResp OpenAI /v1/embeddings 响应体（只解析需要的字段）。
type openAIEmbedResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// EmbedBatch 批量向量化（一次 HTTP 往返，优先使用此方法）。
func (e *OpenAICompatibleEmbeddingAdapter) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(openAIEmbedReq{Model: e.model, Input: texts})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "marshal embed req", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "build embed req", err)
	}
	req.Header.Set("Content-Type", "application/json")
	cleanup := setAuthHeader(req, e.apiKey)
	defer cleanup()

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "embed http", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("embed status %d: %s", resp.StatusCode, raw))
	}

	var out openAIEmbedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "decode embed resp", err)
	}

	// 按 index 排列（Provider 不保证顺序）
	result := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index >= 0 && d.Index < len(result) {
			result[d.Index] = d.Embedding
		}
	}
	return result, nil
}
