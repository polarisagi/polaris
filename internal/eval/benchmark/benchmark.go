package benchmark

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// DatasetConfig 配置数据集的 URL 等信息
type DatasetConfig struct {
	Name string
	URL  string
}

// HumanEvalDatasetURL HumanEval 公开基准数据集的 raw JSONL URL
const HumanEvalDatasetURL = "https://raw.githubusercontent.com/openai/human-eval/master/data/HumanEval.jsonl"

// FetchDataset 下载并解析基准测试数据集为 EvalCase 列表。
// 数据集必须为 JSONL 格式（每行一条 JSON 记录）。
// httpClient 必须由调用方传入（推荐使用 SafeDialer 派生的 Client）；传 nil 将 panic。
func FetchDataset(ctx context.Context, httpClient *http.Client, name string, url string) ([]harness.EvalCase, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "benchmark: build request", err)
	}

	if httpClient == nil {
		panic("benchmark.FetchDataset: httpClient must not be nil (XR-06)")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "benchmark: fetch dataset", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeInternal,
			"benchmark: unexpected status fetching "+name)
	}

	var cases []harness.EvalCase

	// 假设数据集为 JSONL 格式（每行一条记录）
	scanner := bufio.NewScanner(resp.Body)
	// 设置足够大的 buffer 避免行过长截断（单行最大 10 MiB）
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "benchmark: unmarshal JSONL line", err)
		}

		var c harness.EvalCase
		switch name {
		case "humaneval":
			if taskID, ok := raw["task_id"].(string); ok {
				c.ID = taskID
			}
			if prompt, ok := raw["prompt"].(string); ok {
				c.Description = prompt
				c.Input = map[string]any{"prompt": prompt}
			}
			if solution, ok := raw["canonical_solution"].(string); ok {
				c.Expected = map[string]any{"canonical_solution": solution}
			}
			c.Source = "humaneval"
			c.Tags = []string{"coding", "benchmark"}
		default:
			return nil, apperr.New(apperr.CodeInternal, "benchmark: unsupported dataset type: "+name)
		}

		cases = append(cases, c)
	}
	if err := scanner.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "benchmark: reading response body", err)
	}

	return cases, nil
}
