package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin/web_fetch"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockMemoryAutomationService struct {
	mu                      sync.Mutex
	automations             map[string]protocol.PromptAutomationInput
	pipelines               map[string][]byte
	runs                    map[string]string
	agentCount              int
	mockSavePipelineResult  *protocol.PipelineSaveResult
	mockDryRunSpecResult    *protocol.PipelineDryRunResult
	mockDryRunSpecFieldErrs []protocol.FieldError
}

func newMockMemoryAutomationService() *mockMemoryAutomationService {
	return &mockMemoryAutomationService{
		automations: make(map[string]protocol.PromptAutomationInput),
		pipelines:   make(map[string][]byte),
		runs:        make(map[string]string),
	}
}

func (m *mockMemoryAutomationService) List(ctx context.Context) ([]protocol.AutomationSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]protocol.AutomationSummary, 0, len(m.automations)+len(m.pipelines))
	for id, a := range m.automations {
		res = append(res, protocol.AutomationSummary{
			ID:          id,
			Kind:        "prompt",
			Name:        a.Name,
			TriggerType: "cron",
			Cron:        a.Cron,
			Source:      "agent",
			Enabled:     a.Enabled,
		})
	}
	for id := range m.pipelines {
		res = append(res, protocol.AutomationSummary{
			ID:          id,
			Kind:        "pipeline",
			Name:        "pipeline-" + id,
			TriggerType: "cron",
			Cron:        "0 * * * *",
			Source:      "agent",
			Enabled:     true,
			LLMTokens7d: 1234,
		})
	}
	return res, nil
}

func (m *mockMemoryAutomationService) SavePrompt(ctx context.Context, in protocol.PromptAutomationInput) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := in.ID
	if id == "" {
		id = fmt.Sprintf("auto-%d", len(m.automations)+1)
	}
	m.automations[id] = in
	return id, "2026-10-15T00:00:00Z", nil
}

func (m *mockMemoryAutomationService) SavePipeline(ctx context.Context, specJSON []byte, id string) (protocol.PipelineSaveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mockSavePipelineResult != nil {
		return *m.mockSavePipelineResult, nil
	}
	if id == "" {
		id = fmt.Sprintf("wf_%d", len(m.pipelines)+1)
	}
	m.pipelines[id] = specJSON
	return protocol.PipelineSaveResult{
		OK:        true,
		ID:        id,
		NextRunAt: "2026-10-15T00:00:00Z",
	}, nil
}

func (m *mockMemoryAutomationService) DryRunSpec(ctx context.Context, specJSON []byte) (protocol.PipelineDryRunResult, []protocol.FieldError, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mockDryRunSpecResult != nil || len(m.mockDryRunSpecFieldErrs) > 0 {
		res := protocol.PipelineDryRunResult{}
		if m.mockDryRunSpecResult != nil {
			res = *m.mockDryRunSpecResult
		}
		return res, m.mockDryRunSpecFieldErrs, nil
	}
	return protocol.PipelineDryRunResult{
		Steps: []protocol.PipelineDryRunStep{
			{Key: "step1", Kind: "tool", Status: "preview", Count: 1, Preview: []string{"test-preview"}},
		},
		EstimatedLLMInputTokens: 50,
	}, nil, nil
}

func (m *mockMemoryAutomationService) DryRunByID(ctx context.Context, id string) (protocol.PipelineDryRunResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pipelines[id]; !ok {
		return protocol.PipelineDryRunResult{}, apperr.New(apperr.CodeNotFound, "pipeline not found")
	}
	return protocol.PipelineDryRunResult{
		Steps: []protocol.PipelineDryRunStep{
			{Key: "step1", Kind: "tool", Status: "preview", Count: 1, Preview: []string{"test-preview"}},
		},
	}, nil
}

func (m *mockMemoryAutomationService) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.automations[id]; ok {
		delete(m.automations, id)
		return nil
	}
	if _, ok := m.pipelines[id]; ok {
		delete(m.pipelines, id)
		return nil
	}
	return apperr.New(apperr.CodeNotFound, "automation not found")
}

func (m *mockMemoryAutomationService) RunNow(ctx context.Context, id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.automations[id]; !ok {
		if _, okPipe := m.pipelines[id]; !okPipe {
			return "", apperr.New(apperr.CodeNotFound, "automation not found")
		}
	}
	runID := "run-" + id
	m.runs[runID] = id
	return runID, nil
}

func (m *mockMemoryAutomationService) CountEnabledBySource(ctx context.Context, source string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if source == "agent" && m.agentCount > 0 {
		return m.agentCount, nil
	}
	count := 0
	for _, a := range m.automations {
		if a.Enabled {
			count++
		}
	}
	count += len(m.pipelines)
	return count, nil
}

// TestAutomationTools_Lifecycle 验证 save → list → run_now → delete 完整正向生命周期。
func TestAutomationTools_Lifecycle(t *testing.T) {
	svc := newMockMemoryAutomationService()
	ctx := context.Background()

	saveFn := MakeAutomationSaveFn(svc)
	listFn := MakeAutomationListFn(svc)
	runNowFn := MakeAutomationRunNowFn(svc)
	deleteFn := MakeAutomationDeleteFn(svc)

	// 1. Save
	saveIn, _ := json.Marshal(map[string]any{
		"kind":   "prompt",
		"name":   "Hourly Check",
		"prompt": "check server health",
		"cron":   "0 * * * *", // 60m 间隔
	})
	saveOut, err := saveFn(ctx, saveIn)
	if err != nil {
		t.Fatalf("saveFn failed: %v", err)
	}
	var saveRes struct {
		ID        string `json:"id"`
		NextRunAt string `json:"next_run_at"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(saveOut, &saveRes); err != nil {
		t.Fatalf("unmarshal save result: %v", err)
	}
	if saveRes.ID == "" || saveRes.Status != "saved" {
		t.Fatalf("unexpected save result: %+v", saveRes)
	}

	// 2. List
	listOut, err := listFn(ctx, nil)
	if err != nil {
		t.Fatalf("listFn failed: %v", err)
	}
	var listRes struct {
		Automations []protocol.AutomationSummary `json:"automations"`
		Count       int                          `json:"count"`
	}
	if err := json.Unmarshal(listOut, &listRes); err != nil {
		t.Fatalf("unmarshal list result: %v", err)
	}
	if listRes.Count != 1 || listRes.Automations[0].ID != saveRes.ID {
		t.Fatalf("unexpected list result: %+v", listRes)
	}

	// 3. RunNow
	runIn, _ := json.Marshal(map[string]any{
		"id": saveRes.ID,
	})
	runOut, err := runNowFn(ctx, runIn)
	if err != nil {
		t.Fatalf("runNowFn failed: %v", err)
	}
	var runRes struct {
		ID     string `json:"id"`
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(runOut, &runRes); err != nil {
		t.Fatalf("unmarshal run result: %v", err)
	}
	if runRes.RunID != "run-"+saveRes.ID || runRes.Status != "triggered" {
		t.Fatalf("unexpected run result: %+v", runRes)
	}

	// 4. Delete
	delIn, _ := json.Marshal(map[string]any{
		"id": saveRes.ID,
	})
	delOut, err := deleteFn(ctx, delIn)
	if err != nil {
		t.Fatalf("deleteFn failed: %v", err)
	}
	var delRes struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(delOut, &delRes); err != nil {
		t.Fatalf("unmarshal delete result: %v", err)
	}
	if delRes.Status != "deleted" {
		t.Fatalf("unexpected delete result: %+v", delRes)
	}

	// 5. List again, should be empty
	listOut2, _ := listFn(ctx, nil)
	_ = json.Unmarshal(listOut2, &listRes)
	if listRes.Count != 0 {
		t.Fatalf("expected 0 automations after delete, got %d", listRes.Count)
	}
}

// TestAutomationTools_BackgroundContextDenied 验证后台上下文拒绝。
func TestAutomationTools_BackgroundContextDenied(t *testing.T) {
	svc := newMockMemoryAutomationService()
	bgCtx := protocol.WithBackgroundWork(context.Background())

	saveFn := MakeAutomationSaveFn(svc)
	runNowFn := MakeAutomationRunNowFn(svc)
	deleteFn := MakeAutomationDeleteFn(svc)

	saveIn, _ := json.Marshal(map[string]any{
		"kind":   "prompt",
		"prompt": "p",
		"cron":   "0 * * * *",
	})
	if _, err := saveFn(bgCtx, saveIn); !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Errorf("saveFn in background ctx: expected CodeForbidden, got %v", err)
	}

	runIn, _ := json.Marshal(map[string]any{"id": "auto-1"})
	if _, err := runNowFn(bgCtx, runIn); !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Errorf("runNowFn in background ctx: expected CodeForbidden, got %v", err)
	}

	delIn, _ := json.Marshal(map[string]any{"id": "auto-1"})
	if _, err := deleteFn(bgCtx, delIn); !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Errorf("deleteFn in background ctx: expected CodeForbidden, got %v", err)
	}
}

// TestAutomationTools_BackgroundContextThroughRegistry 验证后台标记经 InMemoryToolRegistry.ExecuteTool 可达 InProcessFn。
func TestAutomationTools_BackgroundContextThroughRegistry(t *testing.T) {
	svc := newMockMemoryAutomationService()
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	toolReg := tool.NewInMemoryToolRegistry(
		sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", dummyTokenVerifier{}),
		config.DefaultThresholds().M7Tool,
	)

	if err := RegisterBuiltinTools(sbx, toolReg, []string{"/tmp"}, dummyDialerPtr, false, protocol.NetPolicyDeny, "", &config.Config{}, svc, "", nil, nil, nil, nil, web_fetch.Deps{}); err != nil {
		t.Fatalf("RegisterBuiltinTools failed: %v", err)
	}

	// 注入后台标记与能力令牌
	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityTokenKey{}, &token.Token{})
	bgCtx := protocol.WithBackgroundWork(ctx)

	args, _ := json.Marshal(map[string]any{
		"kind":   "prompt",
		"name":   "test",
		"prompt": "do work",
		"cron":   "0 * * * *",
	})

	res, err := toolReg.ExecuteTool(bgCtx, "automation_save", args, types.TaintNone)
	if err == nil && (res == nil || res.Success) {
		t.Fatalf("expected ExecuteTool to fail with background context, got res=%v err=%v", res, err)
	}
	if err != nil && !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("expected CodeForbidden, got err: %v", err)
	}
}

// TestAutomationTools_ValidationAndLimits 验证参数校验、配额限制与最小调度间隔。
func TestAutomationTools_ValidationAndLimits(t *testing.T) {
	svc := newMockMemoryAutomationService()
	ctx := context.Background()
	saveFn := MakeAutomationSaveFn(svc)

	// 1. 无效的 kind
	invalidKindIn, _ := json.Marshal(map[string]any{"kind": "unknown"})
	if _, err := saveFn(ctx, invalidKindIn); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for unknown kind, got %v", err)
	}

	// 2. prompt 自动化不支持 dry_run
	dryRunIn, _ := json.Marshal(map[string]any{"kind": "prompt", "dry_run": true})
	if _, err := saveFn(ctx, dryRunIn); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for prompt dry_run, got %v", err)
	}

	// 3. prompt 间隔 < 60 分钟被拒绝（如每 30 分钟）
	interval30In, _ := json.Marshal(map[string]any{
		"kind":   "prompt",
		"prompt": "run task",
		"cron":   "*/30 * * * *",
	})
	if _, err := saveFn(ctx, interval30In); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for 30m cron interval, got %v", err)
	}

	// 4. prompt 间隔 >= 60 分钟通过（如每小时）
	interval60In, _ := json.Marshal(map[string]any{
		"kind":   "prompt",
		"prompt": "run task",
		"cron":   "0 * * * *",
	})
	if _, err := saveFn(ctx, interval60In); err != nil {
		t.Errorf("expected success for 60m cron interval, got %v", err)
	}

	// 5. pipeline 间隔 < 15 分钟被拒绝（如每 10 分钟）
	pipelineInterval10In, _ := json.Marshal(map[string]any{
		"kind": "pipeline",
		"cron": "*/10 * * * *",
		"steps": []map[string]any{
			{"key": "fetch", "kind": "tool"},
		},
	})
	if _, err := saveFn(ctx, pipelineInterval10In); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for pipeline 10m cron interval, got %v", err)
	}

	// 6. pipeline 间隔 >= 15 分钟通过（如每 15 分钟）
	pipelineInterval15In, _ := json.Marshal(map[string]any{
		"kind": "pipeline",
		"cron": "*/15 * * * *",
		"steps": []map[string]any{
			{"key": "fetch", "kind": "tool"},
		},
	})
	if _, err := saveFn(ctx, pipelineInterval15In); err != nil {
		t.Errorf("expected success for pipeline 15m cron interval, got %v", err)
	}

	// 7. Agent 配额达上限
	svc.agentCount = 20
	limitIn, _ := json.Marshal(map[string]any{
		"kind":   "prompt",
		"prompt": "run task",
		"cron":   "0 * * * *",
	})
	if _, err := saveFn(ctx, limitIn); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput when agent quota reached, got %v", err)
	}

	// 8. 处于上限时更新已有任务（id != ""）应允许
	updateIn, _ := json.Marshal(map[string]any{
		"id":     "auto-existing",
		"kind":   "prompt",
		"prompt": "updated task",
		"cron":   "0 * * * *",
	})
	if _, err := saveFn(ctx, updateIn); err != nil {
		t.Errorf("expected updating existing automation to succeed even at quota, got %v", err)
	}
}

// TestAutomationTools_PipelineAndDryRun 验证 pipeline 保存、试运行 (dry-run) 与 run_now dry-run 分派。
func TestAutomationTools_PipelineAndDryRun(t *testing.T) {
	svc := newMockMemoryAutomationService()
	ctx := context.Background()
	saveFn := MakeAutomationSaveFn(svc)
	listFn := MakeAutomationListFn(svc)
	runNowFn := MakeAutomationRunNowFn(svc)

	pipelinePayload := map[string]any{
		"kind": "pipeline",
		"name": "HN AI 日报",
		"trigger": map[string]any{
			"type": "cron",
			"cron": "0 8 * * *",
		},
		"steps": []map[string]any{
			{"key": "hn", "kind": "tool"},
		},
	}

	// 1. dry_run=true 预览测试：不落库、返回 ok=true 与 dry_run 结果
	pipelinePayload["dry_run"] = true
	dryIn, _ := json.Marshal(pipelinePayload)
	rawOut, err := saveFn(ctx, dryIn)
	if err != nil {
		t.Fatalf("saveFn dry_run failed: %v", err)
	}
	var dryResp struct {
		OK     bool                          `json:"ok"`
		DryRun protocol.PipelineDryRunResult `json:"dry_run"`
		Errors []protocol.FieldError         `json:"errors"`
	}
	if err := json.Unmarshal(rawOut, &dryResp); err != nil {
		t.Fatalf("unmarshal dry_run response: %v", err)
	}
	if !dryResp.OK || len(dryResp.DryRun.Steps) == 0 {
		t.Errorf("unexpected dry_run response: %+v", dryResp)
	}
	if len(svc.pipelines) != 0 {
		t.Errorf("expected 0 pipelines written during dry_run, got %d", len(svc.pipelines))
	}

	// 2. dry_run=false 正常落库：返回 id (wf_...)
	delete(pipelinePayload, "dry_run")
	saveIn, _ := json.Marshal(pipelinePayload)
	rawOut, err = saveFn(ctx, saveIn)
	if err != nil {
		t.Fatalf("saveFn normal save failed: %v", err)
	}
	var saveResp struct {
		OK        bool                  `json:"ok"`
		ID        string                `json:"id"`
		NextRunAt string                `json:"next_run_at"`
		Errors    []protocol.FieldError `json:"errors"`
	}
	if err := json.Unmarshal(rawOut, &saveResp); err != nil {
		t.Fatalf("unmarshal save response: %v", err)
	}
	if !saveResp.OK || saveResp.ID != "wf_1" {
		t.Errorf("unexpected save response: %+v", saveResp)
	}
	if len(svc.pipelines) != 1 {
		t.Errorf("expected 1 pipeline saved, got %d", len(svc.pipelines))
	}

	// 3. 校验错误返回 ok=false，不返回 apperr
	svc.mockSavePipelineResult = &protocol.PipelineSaveResult{
		OK: false,
		Errors: []protocol.FieldError{
			{Path: "steps[0].tool", Message: "tool not found"},
		},
	}
	rawOut, err = saveFn(ctx, saveIn)
	if err != nil {
		t.Fatalf("saveFn with validation errors should not return error, got: %v", err)
	}
	var errResp struct {
		OK     bool                  `json:"ok"`
		Errors []protocol.FieldError `json:"errors"`
	}
	if err := json.Unmarshal(rawOut, &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.OK || len(errResp.Errors) != 1 || errResp.Errors[0].Path != "steps[0].tool" {
		t.Errorf("expected ok=false with field errors, got %+v", errResp)
	}
	svc.mockSavePipelineResult = nil

	// 4. list 输出包含 kind=pipeline 与 llm_tokens_7d
	rawList, err := listFn(ctx, nil)
	if err != nil {
		t.Fatalf("listFn failed: %v", err)
	}
	var listResp struct {
		Automations []protocol.AutomationSummary `json:"automations"`
		Count       int                          `json:"count"`
	}
	if err := json.Unmarshal(rawList, &listResp); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	var foundPipeline bool
	for _, item := range listResp.Automations {
		if item.Kind == "pipeline" && item.LLMTokens7d == 1234 {
			foundPipeline = true
			break
		}
	}
	if !foundPipeline {
		t.Errorf("expected pipeline summary with tokens in list output, got %+v", listResp)
	}

	// 5. automation_run_now dry_run 分派
	// 5a. wf_ id + dry_run=true -> 成功
	runDryWfIn, _ := json.Marshal(map[string]any{"id": "wf_1", "dry_run": true})
	rawRunDry, err := runNowFn(ctx, runDryWfIn)
	if err != nil {
		t.Fatalf("runNowFn wf_ dry_run failed: %v", err)
	}
	var runDryResp struct {
		OK     bool                          `json:"ok"`
		DryRun protocol.PipelineDryRunResult `json:"dry_run"`
	}
	if err := json.Unmarshal(rawRunDry, &runDryResp); err != nil {
		t.Fatalf("unmarshal runNow dry_run response: %v", err)
	}
	if !runDryResp.OK || len(runDryResp.DryRun.Steps) == 0 {
		t.Errorf("unexpected runNow dry_run response: %+v", runDryResp)
	}

	// 5b. prompt (非 wf_) + dry_run=true -> 拒绝
	runDryPromptIn, _ := json.Marshal(map[string]any{"id": "auto-1", "dry_run": true})
	if _, err := runNowFn(ctx, runDryPromptIn); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for prompt runNow dry_run, got %v", err)
	}
}
