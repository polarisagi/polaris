package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// TrainingSample 单条训练样本（QLoRA / PRM 共用）。
type TrainingSample struct {
	Prompt     string  `json:"prompt"`
	Completion string  `json:"completion"`
	Reward     float64 `json:"reward,omitempty"` // PRM 专用
}

// TrainingResult 训练任务结果。
type TrainingResult struct {
	JobID   string  `json:"job_id"`
	Loss    float64 `json:"loss"`
	Step    int     `json:"step"`
	Adapter string  `json:"adapter_path,omitempty"` // QLoRA adapter 路径
}

// TrainingAdapter 通用训练 HTTP 适配器接口（HTTP Adapter 模式）。
type TrainingAdapter interface {
	Train(ctx context.Context, samples []TrainingSample) (*TrainingResult, error)
}

// ─── QLoRA 适配器 ─────────────────────────────────────────────────────────────

// QLoRAAdapter 对接本地 QLoRA 训练服务（FeatureQLoRA 门控，Tier1+）。
// 默认端点: http://localhost:8000/v1/train/qlora
type QLoRAAdapter struct {
	endpoint string
	client   *http.Client
}

func NewQLoRAAdapter(endpoint string, httpClient *http.Client) *QLoRAAdapter {
	if endpoint == "" {
		endpoint = "http://localhost:8000/v1/train/qlora"
	}
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	return &QLoRAAdapter{endpoint: endpoint, client: httpClient}
}

func (a *QLoRAAdapter) Train(ctx context.Context, samples []TrainingSample) (*TrainingResult, error) {
	if len(samples) == 0 {
		return nil, apperr.New(apperr.CodeInvalidInput, "no training samples")
	}
	body, err := json.Marshal(map[string]any{"samples": samples})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "marshal qlora req", err)
	}
	result, err := postJSON(ctx, a.client, a.endpoint, body)
	if err != nil {
		slog.Warn("inference: QLoRA training service unavailable", "endpoint", a.endpoint, "err", err)
		return nil, fmt.Errorf("QLoRAAdapter.Train: %w", err)
	}
	slog.Info("inference: QLoRA training submitted", "job_id", result.JobID, "step", result.Step)
	return result, nil
}

// ─── PRM 适配器 ───────────────────────────────────────────────────────────────

// PRMAdapter 对接本地 PRM（Process Reward Model）训练服务（FeaturePRMTraining 门控，Tier2）。
// 默认端点: http://localhost:8001/v1/train/prm
type PRMAdapter struct {
	endpoint string
	client   *http.Client
}

func NewPRMAdapter(endpoint string, httpClient *http.Client) *PRMAdapter {
	if endpoint == "" {
		endpoint = "http://localhost:8001/v1/train/prm"
	}
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	return &PRMAdapter{endpoint: endpoint, client: httpClient}
}

func (a *PRMAdapter) Train(ctx context.Context, samples []TrainingSample) (*TrainingResult, error) {
	if len(samples) == 0 {
		return nil, apperr.New(apperr.CodeInvalidInput, "no training samples")
	}
	body, err := json.Marshal(map[string]any{"samples": samples})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "marshal prm req", err)
	}
	result, err := postJSON(ctx, a.client, a.endpoint, body)
	if err != nil {
		slog.Warn("inference: PRM training service unavailable", "endpoint", a.endpoint, "err", err)
		return nil, fmt.Errorf("PRMAdapter.Train: %w", err)
	}
	slog.Info("inference: PRM training submitted", "job_id", result.JobID, "step", result.Step)
	return result, nil
}

// postJSON 公共 JSON POST 辅助。
func postJSON(ctx context.Context, client *http.Client, endpoint string, body []byte) (*TrainingResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "build http req", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "http post", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("training service error %d: %s", resp.StatusCode, raw))
	}

	var result TrainingResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "decode training resp", err)
	}
	return &result, nil
}
