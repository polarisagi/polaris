package hitl

import "context"

// RegressionDetector 轻量回归检测接口（Consumer-side）。
// 实现：eval/regression.LightweightRegressionDetector
type RegressionDetector interface {
	DetectRegression(ctx context.Context, checkpointType string) (*RegressionReport, error)
}

// RegressionReport 回归检测报告（纯数据，复制自 eval/regression.Report）。
type RegressionReport struct {
	Markdown string
	Severity string // "Severe", "Warning", "Minor", "Pass"
}
