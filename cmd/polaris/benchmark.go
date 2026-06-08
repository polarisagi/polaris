package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"context"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/substrate/storage"
)

// promptfooResult 对应 promptfoo --output results.json 的顶层结构。
type promptfooResult struct {
	Results []promptfooEval `json:"results"`
}

type promptfooEval struct {
	Provider   string            `json:"provider"`
	PromptID   string            `json:"promptId"`
	LatencyMs  float64           `json:"latencyMs"`
	Success    bool              `json:"success"`
	TokenUsage *promptfooUsage   `json:"tokenUsage,omitempty"`
	Cost       *float64          `json:"cost,omitempty"`
	Assertions []promptfooAssert `json:"assertions,omitempty"`
}

type promptfooUsage struct {
	PromptTokens     int `json:"prompt"`
	CompletionTokens int `json:"completion"`
	TotalTokens      int `json:"total"`
}

type promptfooAssert struct {
	Pass bool   `json:"pass"`
	Type string `json:"type"`
}

// ─── 子命令入口 ───────────────────────────────────────────────────────────────

// runBenchmarkRouting 执行路由 benchmark 全流程。
// 用法: polaris benchmark-routing <promptfoo_results.json>
func runBenchmarkRouting(args []string) error { //nolint:gocyclo
	if len(args) < 1 {
		return perrors.New(perrors.CodeInvalidInput, "usage: polaris benchmark-routing <promptfoo_results.json> [--help]")
	}

	resultPath := args[0]
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("read promptfoo results: %s", resultPath), err)
	}

	var results promptfooResult
	if err := json.Unmarshal(data, &results); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "parse promptfoo results", err)
	}

	if len(results.Results) == 0 {
		fmt.Println("polaris benchmark: no results from promptfoo")
		return nil
	}

	// ─── 1. 按 provider 聚合统计 ──────────────────────────────────────────────
	type providerStats struct {
		name          string
		count         int
		success       int
		assertPass    int
		totalLatency  float64
		totalCost     float64
		promptTok     int
		completionTok int
	}
	agg := make(map[string]*providerStats)

	for _, r := range results.Results {
		s, ok := agg[r.Provider]
		if !ok {
			s = &providerStats{name: r.Provider}
			agg[r.Provider] = s
		}
		s.count++
		if r.Success {
			s.success++
		}
		for _, a := range r.Assertions {
			if a.Pass {
				s.assertPass++
			}
		}
		s.totalLatency += r.LatencyMs
		if r.Cost != nil {
			s.totalCost += *r.Cost
		}
		if r.TokenUsage != nil {
			s.promptTok += r.TokenUsage.PromptTokens
			s.completionTok += r.TokenUsage.CompletionTokens
		}
	}

	// ─── 2. 计算 HealthScorer 兼容的 ProviderStats ──────────────────────────
	fmt.Println("\n========================================")
	fmt.Println("Polaris — M1 Routing Benchmark")
	fmt.Println("========================================")
	fmt.Printf("%-30s %6s %8s %8s %10s %10s %14s\n",
		"Provider", "Count", "SuccRate", "QualScore", "P95(ms)", "Cost($)", "HealthScore")
	fmt.Println("----------------------------------------")

	type scoredProvider struct {
		name         string
		successRate  float64
		qualityScore float64
		p95Latency   float64
		costAccuracy float64
		healthScore  float64
	}
	var scored []scoredProvider //nolint:prealloc

	// 收集各 provider 的所有延迟用于 P95 计算
	p95Values := make(map[string][]float64)
	for _, r := range results.Results {
		p95Values[r.Provider] = append(p95Values[r.Provider], r.LatencyMs)
	}

	for _, s := range agg {
		successRate := float64(s.success) / float64(s.count)
		qualityScore := float64(s.assertPass) / float64(max(s.count*2, 1)) // 粗略估计: 每 case ~2 assert
		costAccuracy := 1.0
		if s.totalCost > 0 {
			costAccuracy = 1.0 - min(s.totalCost/1.0, 1.0) // cost 归一化到 0-1, $1 为上限
		}

		// P95 计算
		latencies := p95Values[s.name]
		sort.Float64s(latencies)
		p95Idx := int(float64(len(latencies)) * 0.95)
		if p95Idx >= len(latencies) {
			p95Idx = len(latencies) - 1
		}
		p95Latency := latencies[p95Idx]

		// HealthScorer: 可用性×0.4 + 延迟×0.3 + 成本×0.2 + 质量×0.1
		latencyScore := 1.0 - min(p95Latency/5000.0, 1.0)
		healthScore := 0.4*successRate + 0.3*latencyScore + 0.2*costAccuracy + 0.1*qualityScore

		fmt.Printf("%-30s %6d %8.2f %8.2f %8.0f %10.4f %10.4f\n",
			s.name, s.count, successRate, qualityScore, p95Latency, s.totalCost, healthScore)

		scored = append(scored, scoredProvider{
			name:         s.name,
			successRate:  successRate,
			qualityScore: qualityScore,
			p95Latency:   p95Latency,
			costAccuracy: costAccuracy,
			healthScore:  healthScore,
		})
	}
	fmt.Println("----------------------------------------")

	// ─── 3. 按 healthScore 排序 ──────────────────────────────────────────────
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].healthScore > scored[j].healthScore
	})

	fmt.Println("\nRecommended provider order (by HealthScore):")
	for i, p := range scored {
		fmt.Printf("  %d. %s (%.4f)\n", i+1, p.name, p.healthScore)
	}

	// ─── 4. 持久化到 EvalStore (仅在数据库可用时) ────────────────────────────
	cfgPath := os.Getenv("POLARIS_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/defaults.toml"
	}
	_, err = config.Load(cfgPath)
	if err != nil {
		fmt.Printf("polaris benchmark: skip persistence (config load: %v)\n", err)
		return nil
	}

	dataDir, _ := resolveDataDirBase(nil)
	dbPath := filepath.Join(dataDir, "polaris.db")
	store, err := storage.OpenSQLite(dbPath, schema.FS)
	if err != nil {
		fmt.Printf("polaris benchmark: skip persistence (db open: %v)\n", err)
		return nil
	}
	defer store.Close()

	// 将 benchmark 结果写入 decision_log 表
	benchmarkDecision := map[string]any{
		"type":      "routing_benchmark",
		"timestamp": fmt.Sprintf("%d", timeNow()),
		"providers": scored,
	}
	payload, _ := json.Marshal(benchmarkDecision)
	if err := store.Put(noopCtx(), []byte("benchmark:routing:latest"), payload); err != nil {
		fmt.Printf("polaris benchmark: persist warning: %v\n", err)
	}

	fmt.Println("\npolaris benchmark: done — results persisted")
	return nil
}

// max 返回两个 int 的最大值。
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// min 返回两个 float64 的最小值。
func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// timeNow 返回当前 Unix 时间戳（秒）。
func timeNow() int64 {
	return time.Now().Unix()
}

// noopCtx 返回一个永不取消的 context（用于 benchmark 独立模式）。
func noopCtx() context.Context {
	return context.Background()
}
