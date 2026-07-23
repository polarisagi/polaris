package benchmark

import (
	"context"

	"github.com/polarisagi/polaris/internal/eval/harness"
)

// BenchmarkAdapter 定义开放基准数据集向 Polaris 内部 EvalCase 的转换契约。
type BenchmarkAdapter interface {
	Name() string
	Load(ctx context.Context, datasetPath string) ([]harness.EvalCase, error)
}

// GetAdapter returns a benchmark adapter by name.
func GetAdapter(name string) BenchmarkAdapter {
	switch name {
	case "tau-bench":
		return &TauBenchAdapter{}
	case "terminal":
		return &TerminalBenchAdapter{}
	default:
		return nil
	}
}
