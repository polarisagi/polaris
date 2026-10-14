package workflowadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// dryDedupState 只读去重状态（Seen 查库，Pending 丢弃，绝不写库）。
type dryDedupState struct {
	repo repo.WorkflowRepository
	wfID string
}

func (d *dryDedupState) Seen(keys []string) (map[string]bool, error) {
	if d.repo == nil || d.wfID == "" {
		return make(map[string]bool), nil
	}
	seen, err := d.repo.SeenKeys(context.Background(), d.wfID, keys)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "query seen keys in dry run", err)
	}
	return seen, nil
}

func (d *dryDedupState) Pending(keys []string) {
	// dry-run 下丢弃 pending 记录，保证 seen 表不产生脏数据
}

// memoryRunDataStore 纯内存步骤数据暂存器，供试运行在无持久化下跨步骤传递数据。
type memoryRunDataStore struct {
	mu   sync.RWMutex
	data map[string]struct {
		status string
		data   pipeline.StepData
	}
}

func newMemoryRunDataStore() *memoryRunDataStore {
	return &memoryRunDataStore{
		data: make(map[string]struct {
			status string
			data   pipeline.StepData
		}),
	}
}

func (m *memoryRunDataStore) Save(ctx context.Context, runID, key, status string, d pipeline.StepData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = struct {
		status string
		data   pipeline.StepData
	}{status: status, data: d}
	return nil
}

func (m *memoryRunDataStore) Load(ctx context.Context, runID, key string) (string, pipeline.StepData, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.data[key]
	if !ok {
		return "", pipeline.StepData{}, false, nil
	}
	return entry.status, entry.data, true, nil
}

// DryRunPipeline 同步试跑（不落库、不调 LLM、不执行写入类工具、不写 seen），供 Agent 保存前给用户预览。
func (h *WorkflowAdmin) DryRunPipeline(ctx context.Context, spec pipeline.Spec) (protocol.PipelineDryRunResult, []pipeline.FieldError, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	spec = h.resolvePipelineMCPPlaceholders(ctx, spec, "")
	errs := h.validateSpec(spec)
	if len(errs) > 0 {
		return protocol.PipelineDryRunResult{}, errs, nil
	}

	sortedSteps := kahnTopologicalSort(spec.Steps)
	memStore := newMemoryRunDataStore()
	valDeps := toolMetaAdapter{exec: h.ToolExecutor}
	limits := pipeline.Limits{
		MaxSteps:           20,
		MinIntervalMin:     15,
		LLMMaxInputChars:   24000,
		LLMMaxOutputTokens: 2000,
		StepDataMaxBytes:   1048576,
	}

	rc := pipeline.RunContext{
		RunID:      "dry_" + newWorkflowRunID(),
		WorkflowID: "dry_wf",
		Date:       time.Now().UTC().Format("2006-01-02"),
		Since:      time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339),
		Dedup:      &dryDedupState{repo: h.WorkflowRepo, wfID: ""},
		DryRun:     true,
	}

	runner := pipeline.Runner{
		Tools:  toolCallerAdapter{exec: h.ToolExecutor},
		Data:   memStore,
		Limits: limits,
		Deps:   valDeps,
	}

	var result protocol.PipelineDryRunResult
	for _, step := range sortedSteps {
		dryStep, estIn, estOut := h.executeDryRunStep(ctx, runner, memStore, rc, step)
		result.Steps = append(result.Steps, dryStep)
		result.EstimatedLLMInputTokens += estIn
		result.EstimatedLLMOutputTokensMax += estOut
	}

	enforceDryRunResultSizeLimit(&result, 2000)
	return result, nil, nil
}

func (h *WorkflowAdmin) executeDryRunStep(ctx context.Context, runner pipeline.Runner, memStore *memoryRunDataStore, rc pipeline.RunContext, step pipeline.StepSpec) (protocol.PipelineDryRunStep, int, int) {
	if step.Kind == "agent" {
		_ = memStore.Save(ctx, rc.RunID, step.Key, "ok", pipeline.StepData{Count: 0})
		estIn := int(math.Ceil(float64(len([]rune(step.Prompt))) / 2.2))
		return protocol.PipelineDryRunStep{
			Key:    step.Key,
			Kind:   "agent",
			Status: "not_run",
			Note:   "agent 步骤不在试运行中执行",
			Count:  0,
		}, estIn, 2000
	}

	stepRes := runner.RunStep(ctx, rc, step)
	if stepRes.Status == "error" {
		note := "step execution failed"
		if stepRes.Err != nil {
			note = stepRes.Err.Error()
		}
		return protocol.PipelineDryRunStep{
			Key:    step.Key,
			Kind:   step.Kind,
			Status: "error",
			Note:   note,
		}, 0, 0
	}

	if stepRes.Status == "skipped" {
		return protocol.PipelineDryRunStep{
			Key:    step.Key,
			Kind:   step.Kind,
			Status: "skipped",
			Note:   stepRes.Preview,
		}, 0, 0
	}

	previews := extractStepPreviews(stepRes.Data)
	estIn, estOut := 0, 0
	if step.Kind == "llm" {
		estIn, estOut = estimateLLMTokens(step, memStore, rc.RunID)
	}

	return protocol.PipelineDryRunStep{
		Key:     step.Key,
		Kind:    step.Kind,
		Status:  "ok",
		Count:   stepRes.Data.Count,
		Preview: previews,
		Note:    stepRes.Preview,
	}, estIn, estOut
}

// DryRunWorkflow 读取数据库中现有工作流并执行试运行。
func (h *WorkflowAdmin) DryRunWorkflow(ctx context.Context, wfID string) (protocol.PipelineDryRunResult, error) {
	var wf workflow
	var enabledInt int
	err := h.DB.QueryRowContext(ctx, `
		SELECT id, type, name, description, trigger_type, cron_schedule, result_action, deliver_from, enabled
		FROM workflows WHERE id=?`, wfID).Scan(
		&wf.ID, &wf.Type, &wf.Name, &wf.Description, &wf.TriggerType, &wf.CronSchedule,
		&wf.ResultAction, &wf.DeliverFrom, &enabledInt,
	)
	if err != nil {
		return protocol.PipelineDryRunResult{}, apperr.Wrap(apperr.CodeNotFound, "workflow not found", err)
	}
	wf.Enabled = enabledInt == 1

	steps := h.loadWorkflowSteps(ctx, wf.ID)
	spec := pipeline.Spec{
		Kind:         "pipeline",
		Name:         wf.Name,
		Description:  wf.Description,
		Trigger:      pipeline.Trigger{Type: wf.TriggerType, Cron: wf.CronSchedule},
		ResultAction: wf.ResultAction,
		DeliverFrom:  wf.DeliverFrom,
		Steps:        make([]pipeline.StepSpec, 0, len(steps)),
	}

	for i, st := range steps {
		spec.Steps = append(spec.Steps, convertWorkflowStepToPipelineStep(st, i, steps))
	}

	res, errs, err := h.DryRunPipeline(ctx, spec)
	if err != nil {
		return protocol.PipelineDryRunResult{}, err
	}
	if len(errs) > 0 {
		return protocol.PipelineDryRunResult{}, apperr.New(apperr.CodeInvalidInput, formatDryRunFieldErrors(errs))
	}
	return res, nil
}

func formatDryRunFieldErrors(errs []pipeline.FieldError) string {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = fmt.Sprintf("%s: %s", e.Path, e.Message)
	}
	return strings.Join(parts, "; ")
}

func estimateLLMTokens(step pipeline.StepSpec, memStore *memoryRunDataStore, runID string) (int, int) {
	inputChars := len([]rune(step.Prompt)) + 100 // system prompt approx
	if step.Input != "" {
		_, inData, ok, _ := memStore.Load(context.Background(), runID, step.Input)
		if ok {
			inputChars += len([]rune(inData.Text))
			for _, it := range inData.Items {
				for _, v := range it {
					inputChars += len([]rune(fmt.Sprint(v)))
				}
			}
		}
	}
	estIn := int(math.Ceil(float64(inputChars) / 2.2))
	estOut := step.MaxOutputTokens
	if estOut <= 0 {
		estOut = 2000
	}
	return estIn, estOut
}

func extractStepPreviews(data pipeline.StepData) []string {
	var previews []string
	if len(data.Items) > 0 {
		limit := len(data.Items)
		if limit > 3 {
			limit = 3
		}
		for i := 0; i < limit; i++ {
			text := extractItemPreviewText(data.Items[i])
			if text != "" {
				previews = append(previews, truncateRunes(text, 120))
			}
		}
	} else if strings.TrimSpace(data.Text) != "" {
		previews = append(previews, truncateRunes(strings.TrimSpace(data.Text), 120))
	}
	return previews
}

func extractItemPreviewText(item map[string]any) string {
	if val, ok := item["title"]; ok && val != nil {
		if s := fmt.Sprint(val); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if val, ok := item["url"]; ok && val != nil {
		if s := fmt.Sprint(val); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	for k, v := range item {
		if v != nil {
			s := fmt.Sprint(v)
			if strings.TrimSpace(s) != "" {
				return fmt.Sprintf("%s: %s", k, strings.TrimSpace(s))
			}
		}
	}
	return ""
}

func enforceDryRunResultSizeLimit(res *protocol.PipelineDryRunResult, maxRunes int) {
	for {
		b, err := json.Marshal(res)
		if err != nil || len([]rune(string(b))) <= maxRunes {
			return
		}
		reduced := false
		for i := len(res.Steps) - 1; i >= 0; i-- {
			if len(res.Steps[i].Preview) > 0 {
				res.Steps[i].Preview = res.Steps[i].Preview[:len(res.Steps[i].Preview)-1]
				reduced = true
				break
			}
		}
		if !reduced {
			return
		}
	}
}

func kahnTopologicalSort(steps []pipeline.StepSpec) []pipeline.StepSpec {
	stepMap := make(map[string]pipeline.StepSpec, len(steps))
	inDegree := make(map[string]int, len(steps))
	adj := make(map[string][]string, len(steps))

	for _, s := range steps {
		stepMap[s.Key] = s
		inDegree[s.Key] = len(s.Needs)
		for _, need := range s.Needs {
			adj[need] = append(adj[need], s.Key)
		}
	}

	queue := make([]string, 0, len(steps))
	for _, s := range steps {
		if inDegree[s.Key] == 0 {
			queue = append(queue, s.Key)
		}
	}

	var sorted []pipeline.StepSpec
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		if s, ok := stepMap[curr]; ok {
			sorted = append(sorted, s)
		}
		for _, dep := range adj[curr] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	if len(sorted) != len(steps) {
		return steps // fallback
	}
	return sorted
}
