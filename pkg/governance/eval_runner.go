package governance

import (
	"fmt"
	"time"
)

// RunnerImpl 评测执行器实现
type RunnerImpl struct {
	evaluators []Evaluator
	agent      AgentRunner
}

// NewRunnerImpl 创建评测执行器
func NewRunnerImpl(agent AgentRunner) *RunnerImpl {
	return &RunnerImpl{
		agent: agent,
		evaluators: []Evaluator{
			&L1AssertionEvaluator{},
			&L2SchemaEvaluator{},
			&L3TrajectoryEvaluator{},
			&L4LLMJudgeEvaluator{},
			&L5HumanEvaluator{},
		},
	}
}

// Run 执行评测集合并生成报告
func (r *RunnerImpl) Run(cases []*EvalCase) *EvalRunReport { //nolint:gocyclo,nestif
	report := &EvalRunReport{
		RunID:      fmt.Sprintf("run-%d", time.Now().Unix()),
		TotalCases: len(cases),
	}

	p0Total, p0Passed := 0, 0
	p1Total, p1Passed := 0, 0

	for _, c := range cases {
		// Mock trajectory processing for MVP
		traj := &AgentTrajectory{
			Result: &TrajectoryResult{Output: "{}", Success: true},
		}

		passed := true
		for _, ev := range r.evaluators {
			requested := false
			for _, spec := range c.Evaluators {
				if spec.Type == ev.Type() {
					requested = true
					break
				}
			}
			if !requested {
				continue
			}

			res, err := ev.Evaluate(traj, c)
			if err != nil || !res.Passed {
				passed = false
				break
			}
		}

		if passed {
			report.PassedCases++
		} else {
			report.FailedCases++
		}

		if c.Severity == 0 {
			p0Total++
			if passed {
				p0Passed++
			}
		} else if c.Severity == 1 {
			p1Total++
			if passed {
				p1Passed++
			}
		}
	}

	if report.TotalCases > 0 {
		report.PassRate = float64(report.PassedCases) / float64(report.TotalCases)
	}
	if p0Total > 0 {
		report.P0PassRate = float64(p0Passed) / float64(p0Total)
	}
	if p1Total > 0 {
		report.P1PassRate = float64(p1Passed) / float64(p1Total)
	}

	report.BlockDeploy = (report.P0PassRate < 1.0)
	report.WarnDeploy = (report.P1PassRate < 0.95)

	return report
}
