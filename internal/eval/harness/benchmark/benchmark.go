package benchmark

import (
	"github.com/polarisagi/polaris/internal/protocol"
)

// BenchmarkAdapter 定义开放基准数据集向 Polaris 内部 EvalCase 的转换契约。
type BenchmarkAdapter = protocol.BenchmarkAdapter

// GetAdapter returns a benchmark adapter by name.
func GetAdapter(name string) BenchmarkAdapter {
	switch name {
	case "tau-bench":
		return &TauBenchAdapter{}
	case "terminal":
		return &TerminalBenchAdapter{}
	case "locomo":
		return &LoCoMoAdapter{}
	case "longmemeval":
		return &LongMemEvalAdapter{}
	case "swe-bench":
		return &SWEBenchLiteAdapter{}
	case "swe-bench-jsonl":
		return &SWEBenchLiteJSONLAdapter{}
	case "gaia":
		return &GAIAAdapter{}
	default:
		return nil
	}
}
