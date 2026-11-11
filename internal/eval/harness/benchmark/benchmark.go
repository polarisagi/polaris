package benchmark

import (
	"io"
	"os"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const maxBenchmarkDatasetBytes = 10 * 1024 * 1024

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
	case "project":
		return &ProjectAdapter{}
	case "gaia":
		return &GAIAAdapter{}
	default:
		return nil
	}
}

func readBoundedDataset(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "benchmark: open dataset", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "benchmark: stat dataset", err)
	}
	if !info.Mode().IsRegular() {
		return nil, apperr.New(apperr.CodeInvalidInput, "benchmark: dataset must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBenchmarkDatasetBytes+1))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "benchmark: read dataset", err)
	}
	if len(data) > maxBenchmarkDatasetBytes {
		return nil, apperr.New(apperr.CodeInvalidInput, "benchmark: dataset exceeds 10 MiB")
	}
	return data, nil
}
