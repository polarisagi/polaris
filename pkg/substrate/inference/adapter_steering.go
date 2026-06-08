package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// SteeringAdapter 对接本地激活引导服务（FeatureActivationSteer 门控，Tier1+）。
// 通过注入 hidden state 差向量实现行为引导，兼容 llama.cpp 扩展接口。
// 默认端点: http://localhost:8080/v1/steer
type SteeringAdapter struct {
	endpoint string
	client   *http.Client
}

// SteerRequest 激活引导请求。
type SteerRequest struct {
	// Layer 注入层索引（llama 模型通常在中间层效果最佳）
	Layer int `json:"layer"`
	// Vector 引导向量（float32，维度需与模型 hidden_size 对齐）
	Vector []float32 `json:"vector"`
	// Scale 引导强度（推荐 10~30；>50 可能导致解码退化）
	Scale float64 `json:"scale"`
	// SessionID 会话 ID（可选，用于持久化引导状态）
	SessionID string `json:"session_id,omitempty"`
}

// SteerResponse 激活引导响应。
type SteerResponse struct {
	Applied bool   `json:"applied"`
	Layer   int    `json:"layer"`
	Message string `json:"message,omitempty"`
}

func NewSteeringAdapter(endpoint string, httpClient *http.Client) *SteeringAdapter {
	if endpoint == "" {
		endpoint = "http://localhost:8080/v1/steer"
	}
	if httpClient == nil {
		httpClient = defaultHTTPClient
	}
	return &SteeringAdapter{endpoint: endpoint, client: httpClient}
}

// SteerActivations 向运行中的模型注入激活引导向量。
func (a *SteeringAdapter) SteerActivations(ctx context.Context, req *SteerRequest) (*SteerResponse, error) {
	if req == nil || len(req.Vector) == 0 {
		return nil, perrors.New(perrors.CodeInvalidInput, "steering vector is empty")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "marshal steer req", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "build steer req", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		slog.Warn("inference: activation steering service unavailable", "endpoint", a.endpoint, "err", err)
		return nil, perrors.Wrap(perrors.CodeInternal, "steer http", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("steer service error %d: %s", resp.StatusCode, raw))
	}

	var out SteerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "decode steer resp", err)
	}
	slog.Debug("inference: activation steering applied", "layer", out.Layer, "applied", out.Applied)
	return &out, nil
}

// ClearSteering 清除会话的激活引导状态。
func (a *SteeringAdapter) ClearSteering(ctx context.Context, sessionID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/%s", a.endpoint, sessionID), nil)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "build clear steer req", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "clear steer http", err)
	}
	defer resp.Body.Close()
	return nil
}
