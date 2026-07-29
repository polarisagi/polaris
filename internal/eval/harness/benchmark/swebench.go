package benchmark

import (
	"bufio"
	"context"
	"encoding/json"
	"os"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SWEBenchLiteAdapter 实现 BenchmarkAdapter，加载 SWE-bench Lite 标准 JSON 格式。
// 数据格式（JSON 数组）：
//
//	[{"instance_id":"...", "repo":"...", "base_commit":"...",
//	  "patch":"...", "test_patch":"...", "problem_statement":"...",
//	  "FAIL_TO_PASS":["test_a","test_b"], "PASS_TO_PASS":["test_c"]}]
//
// 注意：Adapter 只负责数据转换，代码执行（git apply + 测试套件）由 Runner 负责。
type SWEBenchLiteAdapter struct{}

func (a *SWEBenchLiteAdapter) Name() string { return "swe-bench" }

// sweBenchInstance 对应 SWE-bench Lite 数据集的单条记录结构。
type sweBenchInstance struct {
	InstanceID       string   `json:"instance_id"`
	Repo             string   `json:"repo"`
	BaseCommit       string   `json:"base_commit"`
	Patch            string   `json:"patch"`
	TestPatch        string   `json:"test_patch"`
	ProblemStatement string   `json:"problem_statement"`
	FailToPass       []string `json:"FAIL_TO_PASS"`
	PassToPass       []string `json:"PASS_TO_PASS"`
}

// Load 读取 SWE-bench Lite JSON 文件（数组格式），映射为 EvalCase 列表。
func (a *SWEBenchLiteAdapter) Load(_ context.Context, datasetPath string) ([]protocol.EvalCase, error) {
	data, err := os.ReadFile(datasetPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "SWEBenchLiteAdapter: read dataset", err)
	}

	var instances []sweBenchInstance
	if err := json.Unmarshal(data, &instances); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "SWEBenchLiteAdapter: parse dataset", err)
	}

	cases := make([]protocol.EvalCase, 0, len(instances))
	for _, inst := range instances {
		cases = append(cases, protocol.EvalCase{
			ID:          inst.InstanceID,
			Description: inst.ProblemStatement,
			Input: map[string]any{
				"repo":        inst.Repo,
				"base_commit": inst.BaseCommit,
				"test_patch":  inst.TestPatch,
			},
			Expected: map[string]any{
				"patch":         inst.Patch,
				"fail_to_pass":  inst.FailToPass,
				"pass_to_pass":  inst.PassToPass,
			},
			BehaviorType:        protocol.BehaviorCodePatch,
			Level:               protocol.Level3Trajectory,
			FalsifiabilityScore: 0.9,
			Severity:            protocol.SeverityP1,
			Source:              "swebench-lite",
			Tags:                []string{"benchmark", "swe-bench", "coding"},
		})
	}

	return cases, nil
}

// SWEBenchLiteJSONLAdapter 支持 JSONL（每行一条记录）格式的 SWE-bench Lite 数据集。
// 注意：官方发布的 SWE-bench Lite 是 JSON 数组格式；此实现兼容某些镜像站点的 JSONL 导出。
type SWEBenchLiteJSONLAdapter struct{}

func (a *SWEBenchLiteJSONLAdapter) Name() string { return "swe-bench-jsonl" }

// Load 读取 JSONL 格式 SWE-bench 数据集。
func (a *SWEBenchLiteJSONLAdapter) Load(_ context.Context, datasetPath string) ([]protocol.EvalCase, error) {
	f, err := os.Open(datasetPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "SWEBenchLiteJSONLAdapter: open dataset", err)
	}
	defer f.Close()

	var cases []protocol.EvalCase
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		var inst sweBenchInstance
		if err := json.Unmarshal(scanner.Bytes(), &inst); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "SWEBenchLiteJSONLAdapter: parse line", err)
		}
		cases = append(cases, protocol.EvalCase{
			ID:          inst.InstanceID,
			Description: inst.ProblemStatement,
			Input: map[string]any{
				"repo":        inst.Repo,
				"base_commit": inst.BaseCommit,
				"test_patch":  inst.TestPatch,
			},
			Expected: map[string]any{
				"patch":        inst.Patch,
				"fail_to_pass": inst.FailToPass,
				"pass_to_pass": inst.PassToPass,
			},
			BehaviorType:        protocol.BehaviorCodePatch,
			Level:               protocol.Level3Trajectory,
			FalsifiabilityScore: 0.9,
			Severity:            protocol.SeverityP1,
			Source:              "swebench-lite",
			Tags:                []string{"benchmark", "swe-bench", "coding"},
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SWEBenchLiteJSONLAdapter: scan", err)
	}

	return cases, nil
}
