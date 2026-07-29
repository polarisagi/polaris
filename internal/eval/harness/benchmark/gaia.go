package benchmark

import (
	"bufio"
	"context"
	"encoding/json"
	"os"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// GAIAAdapter 实现 BenchmarkAdapter，加载 GAIA 数据集（metadata.jsonl 格式）。
// 官方数据格式（JSONL，每行一条）：
//
//	{"task_id":"...", "Question":"...", "Level":1, "Final answer":"...", "Annotator Metadata":{...}}
//
// Level 映射规则：
//
//	Level 1 → Level1Assert  (确定性答案匹配)
//	Level 2 → Level2Schema  (答案带结构约束)
//	Level 3 → Level3Trajectory (需要多步推理轨迹)
type GAIAAdapter struct{}

func (a *GAIAAdapter) Name() string { return "gaia" }

// gaiaTask 对应 GAIA metadata.jsonl 单行结构。
type gaiaTask struct {
	TaskID      string `json:"task_id"`
	Question    string `json:"Question"`
	Level       int    `json:"Level"`
	FinalAnswer string `json:"Final answer"`
}

// Load 读取 GAIA JSONL 文件，映射为 EvalCase 列表。
func (a *GAIAAdapter) Load(_ context.Context, datasetPath string) ([]protocol.EvalCase, error) {
	f, err := os.Open(datasetPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "GAIAAdapter: open dataset", err)
	}
	defer f.Close()

	var cases []protocol.EvalCase
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 512*1024), 4*1024*1024)
	for scanner.Scan() {
		var t gaiaTask
		if err := json.Unmarshal(scanner.Bytes(), &t); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "GAIAAdapter: parse line", err)
		}

		level, falsifiability := gaiaLevelToEval(t.Level)

		cases = append(cases, protocol.EvalCase{
			ID:          t.TaskID,
			Description: t.Question,
			Input:       map[string]any{"question": t.Question},
			Expected:    map[string]any{"answer": t.FinalAnswer},
			BehaviorType:        protocol.BehaviorFinalAnswerMatch,
			Level:               level,
			FalsifiabilityScore: falsifiability,
			Severity:            gaiaSeverity(t.Level),
			Source:              "gaia",
			Tags:                []string{"benchmark", "gaia", gaiaLevelTag(t.Level)},
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "GAIAAdapter: scan", err)
	}

	return cases, nil
}

// gaiaLevelToEval 将 GAIA 难度（1/2/3）映射到内部 EvaluatorLevel 和可评分性分数。
func gaiaLevelToEval(gaiaLevel int) (protocol.EvaluatorLevel, float64) {
	switch gaiaLevel {
	case 1:
		// Level 1：简单单步问题，答案唯一确定
		return protocol.Level1Assert, 1.0
	case 2:
		// Level 2：中等复杂度，需要工具使用+结构化输出验证
		return protocol.Level2Schema, 0.85
	default:
		// Level 3：需要多步推理，验证轨迹而非单一答案
		return protocol.Level3Trajectory, 0.7
	}
}

// gaiaSeverity 将 GAIA Level 映射到测试严重度。
func gaiaSeverity(gaiaLevel int) protocol.Severity {
	if gaiaLevel <= 1 {
		return protocol.SeverityP0 // Level 1 答案确定，未通过即为明显 bug
	}
	if gaiaLevel == 2 {
		return protocol.SeverityP1
	}
	return protocol.SeverityP2
}

func gaiaLevelTag(level int) string {
	switch level {
	case 1:
		return "level-1"
	case 2:
		return "level-2"
	default:
		return "level-3"
	}
}
