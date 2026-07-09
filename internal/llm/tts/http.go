package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// HTTPProvider 将 TTS 请求转发给外部 HTTP 服务（Python sidecar）。
//
// 约定接口：
//
//	POST {endpoint}
//	Content-Type: application/json
//	Body: {"text": "要合成的文本"}
//
//	Response: 200 OK
//	Content-Type: audio/wav
//	Body: WAV 字节流
//
// 推荐 sidecar：
//   - CosyVoice 2（阿里达摩院，中文顶级质量，需 GPU）
//   - Qwen3-TTS（通义千问 TTS，中文极佳，需 GPU）
//   - GPT-SoVITS（开源，CPU 可用但较慢）
type HTTPProvider struct {
	endpoint   string
	httpClient *http.Client
}

// NewHTTPProvider 返回指向 endpoint 的 HTTP TTS 适配器。
// client 必须由调用方注入（通常是 substrate.NewSafeHTTPClient），禁止传 nil（XR-06）。
func NewHTTPProvider(endpoint string, client *http.Client) *HTTPProvider {
	if client == nil {
		// XR-06 fail-closed：禁止降级到 DefaultClient，要求调用方显式提供隔离客户端。
		panic("tts.NewHTTPProvider: httpClient is required; use substrate.NewSafeHTTPClient (XR-06)")
	}
	return &HTTPProvider{endpoint: endpoint, httpClient: client}
}

// Generate 向 sidecar 发送合成请求并返回 WAV 字节流。
func (p *HTTPProvider) Generate(ctx context.Context, text string) ([]byte, error) {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "http-tts: marshal failed", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "http-tts: build request failed", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/wav")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "http-tts: request failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("http-tts: sidecar returned HTTP %d", resp.StatusCode))
	}

	wav, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "http-tts: read response failed", err)
	}
	if len(wav) == 0 {
		return nil, apperr.New(apperr.CodeInternal, "http-tts: sidecar returned empty body")
	}
	return wav, nil
}

// Close 实现 Provider 接口（HTTPProvider 无持久连接，空操作）。
func (p *HTTPProvider) Close() error { return nil }
