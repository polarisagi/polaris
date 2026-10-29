package pipeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// fakeDataStore 内存实现的 RunDataStore。
type fakeDataStore struct {
	data   map[string]map[string]pipeline.StepData // runID -> key -> data
	status map[string]map[string]string            // runID -> key -> status
}

func newFakeDataStore() *fakeDataStore {
	return &fakeDataStore{
		data:   make(map[string]map[string]pipeline.StepData),
		status: make(map[string]map[string]string),
	}
}

func (s *fakeDataStore) Save(ctx context.Context, runID, key, status string, d pipeline.StepData) error {
	if _, ok := s.data[runID]; !ok {
		s.data[runID] = make(map[string]pipeline.StepData)
		s.status[runID] = make(map[string]string)
	}
	s.data[runID][key] = d
	s.status[runID][key] = status
	return nil
}

func (s *fakeDataStore) Load(ctx context.Context, runID, key string) (status string, d pipeline.StepData, ok bool, err error) {
	runs, hasRun := s.data[runID]
	if !hasRun {
		return "", pipeline.StepData{}, false, nil
	}
	d, ok = runs[key]
	if !ok {
		return "", pipeline.StepData{}, false, nil
	}
	return s.status[runID][key], d, true, nil
}

// fakeToolCaller 记录调用的参数与污点级别。
type fakeToolCaller struct {
	calls []struct {
		name      string
		args      string
		highTaint bool
	}
	handler func(ctx context.Context, name string, args []byte, highTaint bool) ([]byte, error)
}

func (c *fakeToolCaller) Call(ctx context.Context, name string, args []byte, highTaint bool) ([]byte, error) {
	c.calls = append(c.calls, struct {
		name      string
		args      string
		highTaint bool
	}{name: name, args: string(args), highTaint: highTaint})
	if c.handler != nil {
		return c.handler(ctx, name, args, highTaint)
	}
	return []byte(`{"items": [{"title": "t1", "url": "https://example.com/1"}]}`), nil
}

// fakeLLMCaller 记录 LLM 调用请求。
type fakeLLMCaller struct {
	requests []pipeline.LLMRequest
	handler  func(ctx context.Context, req pipeline.LLMRequest) (string, error)
}

func (c *fakeLLMCaller) Complete(ctx context.Context, req pipeline.LLMRequest) (string, error) {
	c.requests = append(c.requests, req)
	if c.handler != nil {
		return c.handler(ctx, req)
	}
	return "LLM summary result", nil
}

type fakeValidateDeps struct {
	metas map[string]pipeline.ToolMeta
}

func (d *fakeValidateDeps) ToolMeta(name string) pipeline.ToolMeta {
	if m, ok := d.metas[name]; ok {
		return m
	}
	return pipeline.ToolMeta{Name: name, ReadOnly: true, Exists: true}
}

// TestRunner_ToolTransformLLMChain 测试 tool -> transform -> llm 完整链路成功执行。
func TestRunner_ToolTransformLLMChain(t *testing.T) {
	store := newFakeDataStore()
	toolCaller := &fakeToolCaller{
		handler: func(ctx context.Context, name string, args []byte, highTaint bool) ([]byte, error) {
			raw := `{"items": [
				{"title": "Go 1.25 Released", "url": "https://go.dev/1", "summary": "Release notes"},
				{"title": "Rust 2026", "url": "https://rust-lang.org/1", "summary": "Rust news"}
			]}`
			return []byte(raw), nil
		},
	}
	llmCaller := &fakeLLMCaller{
		handler: func(ctx context.Context, req pipeline.LLMRequest) (string, error) {
			if !strings.Contains(req.User, "Go 1.25") {
				return "", fmt.Errorf("user prompt missing expected data: %s", req.User)
			}
			return "今日重要更新：Go 1.25 发布。", nil
		},
	}

	runner := &pipeline.Runner{
		Tools:  toolCaller,
		LLM:    llmCaller,
		Data:   store,
		Limits: pipeline.Limits{StepDataMaxBytes: 1048576, LLMMaxInputChars: 24000, LLMMaxOutputTokens: 2000},
	}
	rc := pipeline.RunContext{
		RunID:      "run-101",
		WorkflowID: "wf-101",
		Date:       "2026-10-14",
		Since:      "2026-10-13T00:00:00Z",
	}

	// 步骤 1: tool 抓取
	toolStep := pipeline.StepSpec{
		Key:  "fetch",
		Kind: "tool",
		Tool: "feed_fetch",
		Args: json.RawMessage(`{"url": "https://example.com/rss"}`),
	}
	res1 := runner.RunStep(context.Background(), rc, toolStep)
	if res1.Status != "ok" || res1.Err != nil {
		t.Fatalf("tool step failed: %v", res1.Err)
	}
	if res1.Data.Count != 2 {
		t.Fatalf("expected 2 items, got %d", res1.Data.Count)
	}
	if !strings.Contains(res1.Preview, "count=2") || !strings.Contains(res1.Preview, "Go 1.25 Released") {
		t.Errorf("unexpected preview: %s", res1.Preview)
	}

	// 步骤 2: transform 过滤只保留 Go
	transformStep := pipeline.StepSpec{
		Key:   "filter",
		Kind:  "transform",
		Needs: []string{"fetch"},
		Ops: []pipeline.Op{
			{Op: "filter", Field: "title", Cmp: "contains", Value: "Go"},
		},
	}
	res2 := runner.RunStep(context.Background(), rc, transformStep)
	if res2.Status != "ok" || res2.Err != nil {
		t.Fatalf("transform step failed: %v", res2.Err)
	}
	if res2.Data.Count != 1 {
		t.Fatalf("expected 1 item, got %d", res2.Data.Count)
	}

	// 步骤 3: llm 总结
	llmStep := pipeline.StepSpec{
		Key:    "summary",
		Kind:   "llm",
		Needs:  []string{"filter"},
		Input:  "filter",
		Prompt: "请将下列信息整理为日报简报：",
	}
	res3 := runner.RunStep(context.Background(), rc, llmStep)
	if res3.Status != "ok" || res3.Err != nil {
		t.Fatalf("llm step failed: %v", res3.Err)
	}
	if !strings.Contains(res3.Data.Text, "今日重要更新：Go 1.25 发布。") {
		t.Errorf("unexpected llm text: %s", res3.Data.Text)
	}
	if len(llmCaller.requests) != 1 {
		t.Fatalf("expected 1 llm request, got %d", len(llmCaller.requests))
	}
}

// TestRunner_SkippedDependencyPropagation 测试跳过依赖的向下传递。
func TestRunner_SkippedDependencyPropagation(t *testing.T) {
	store := newFakeDataStore()
	runner := &pipeline.Runner{
		Data: store,
	}
	rc := pipeline.RunContext{RunID: "run-skip-test"}

	// 预设上游步骤为 skipped
	_ = store.Save(context.Background(), "run-skip-test", "prev_step", "skipped", pipeline.StepData{})

	downstream := pipeline.StepSpec{
		Key:   "next_step",
		Kind:  "transform",
		Needs: []string{"prev_step"},
		Ops:   []pipeline.Op{{Op: "limit", N: 5}},
	}

	res := runner.RunStep(context.Background(), rc, downstream)
	if res.Status != "skipped" {
		t.Fatalf("expected skipped status, got %s", res.Status)
	}
	if !strings.Contains(res.Preview, "prev_step was skipped") {
		t.Errorf("expected skip reason in preview, got %s", res.Preview)
	}

	// 确认库中记录亦为 skipped
	savedStatus, _, ok, _ := store.Load(context.Background(), "run-skip-test", "next_step")
	if !ok || savedStatus != "skipped" {
		t.Errorf("expected next_step to be saved as skipped, got ok=%v, status=%s", ok, savedStatus)
	}
}

// TestRunner_SkipIfEmpty 测试 skip_if_empty 规则。
func TestRunner_SkipIfEmpty(t *testing.T) {
	store := newFakeDataStore()
	toolCaller := &fakeToolCaller{
		handler: func(ctx context.Context, name string, args []byte, highTaint bool) ([]byte, error) {
			return []byte(`{"items": []}`), nil
		},
	}
	runner := &pipeline.Runner{
		Tools: toolCaller,
		Data:  store,
	}
	rc := pipeline.RunContext{RunID: "run-empty-test"}

	// 1. tool 步骤，未指定 skip_if_empty -> status="ok", count=0
	s1 := pipeline.StepSpec{
		Key:  "fetch_empty",
		Kind: "tool",
		Tool: "feed_fetch",
		Args: json.RawMessage(`{}`),
	}
	r1 := runner.RunStep(context.Background(), rc, s1)
	if r1.Status != "ok" {
		t.Errorf("expected ok status when skip_if_empty is false, got %s", r1.Status)
	}

	// 2. tool 步骤，指定 skip_if_empty -> status="skipped"
	s2 := pipeline.StepSpec{
		Key:         "fetch_empty_skip",
		Kind:        "tool",
		Tool:        "feed_fetch",
		Args:        json.RawMessage(`{}`),
		SkipIfEmpty: true,
	}
	r2 := runner.RunStep(context.Background(), rc, s2)
	if r2.Status != "skipped" {
		t.Errorf("expected skipped status when skip_if_empty is true and count=0, got %s", r2.Status)
	}
	if r2.Preview != "skipped (empty items)" {
		t.Errorf("unexpected preview: %s", r2.Preview)
	}

	// 3. llm 步骤，即便指定 skip_if_empty 也不适用（llm 结果非 items 集合）
	llmCaller := &fakeLLMCaller{
		handler: func(ctx context.Context, req pipeline.LLMRequest) (string, error) {
			return "no news today", nil
		},
	}
	runner.LLM = llmCaller
	_ = store.Save(context.Background(), "run-empty-test", "input_step", "ok", pipeline.StepData{Items: []map[string]any{}})

	s3 := pipeline.StepSpec{
		Key:         "llm_step",
		Kind:        "llm",
		Needs:       []string{"input_step"},
		Input:       "input_step",
		Prompt:      "总结：",
		SkipIfEmpty: true,
	}
	r3 := runner.RunStep(context.Background(), rc, s3)
	if r3.Status != "ok" {
		t.Errorf("llm step should not be skipped by skip_if_empty, got %s", r3.Status)
	}
}

// TestRunner_TaintPropagationByRefs 测试静态参数为 low taint，引用参数为 high taint。
func TestRunner_TaintPropagationByRefs(t *testing.T) {
	store := newFakeDataStore()
	toolCaller := &fakeToolCaller{}
	runner := &pipeline.Runner{
		Tools: toolCaller,
		Data:  store,
	}
	rc := pipeline.RunContext{RunID: "run-taint-test"}

	// 1. 静态参数 -> highTaint = false
	s1 := pipeline.StepSpec{
		Key:  "static_tool",
		Kind: "tool",
		Tool: "web_fetch",
		Args: json.RawMessage(`{"url": "https://example.com"}`),
	}
	res1 := runner.RunStep(context.Background(), rc, s1)
	if res1.Status != "ok" {
		t.Fatalf("static tool step failed: %v", res1.Err)
	}
	if toolCaller.calls[0].highTaint {
		t.Errorf("expected highTaint=false for static tool args, got true")
	}

	// 2. 引用上游数据 -> highTaint = true
	_ = store.Save(context.Background(), "run-taint-test", "prev_fetch", "ok", pipeline.StepData{
		Text: "https://example.com/dynamic",
	})
	s2 := pipeline.StepSpec{
		Key:   "ref_tool",
		Kind:  "tool",
		Tool:  "web_fetch",
		Needs: []string{"prev_fetch"},
		Args:  json.RawMessage(`{"url": "{{steps.prev_fetch.text}}"}`),
	}
	res2 := runner.RunStep(context.Background(), rc, s2)
	if res2.Status != "ok" {
		t.Fatalf("ref tool step failed: %v", res2.Err)
	}
	if !toolCaller.calls[1].highTaint {
		t.Errorf("expected highTaint=true for ref-based tool args, got false")
	}
}

// TestRunner_DryRun 测试 DryRun 模式下工具与 LLM 的行为。
func TestRunner_DryRun(t *testing.T) {
	store := newFakeDataStore()
	toolCaller := &fakeToolCaller{}
	llmCaller := &fakeLLMCaller{}
	deps := &fakeValidateDeps{
		metas: map[string]pipeline.ToolMeta{
			"feed_fetch":       {Name: "feed_fetch", ReadOnly: true, Exists: true},
			"knowledge_ingest": {Name: "knowledge_ingest", WriteAllowed: true, Exists: true},
		},
	}

	runner := &pipeline.Runner{
		Tools:  toolCaller,
		LLM:    llmCaller,
		Data:   store,
		Deps:   deps,
		Limits: pipeline.Limits{LLMMaxInputChars: 24000, LLMMaxOutputTokens: 2000},
	}
	rc := pipeline.RunContext{
		RunID:  "run-dry-run",
		DryRun: true,
	}

	// 1. 只读工具（feed_fetch）正常调用
	readStep := pipeline.StepSpec{
		Key:  "read_step",
		Kind: "tool",
		Tool: "feed_fetch",
		Args: json.RawMessage(`{"url": "https://example.com"}`),
	}
	resRead := runner.RunStep(context.Background(), rc, readStep)
	if resRead.Status != "ok" {
		t.Fatalf("read step failed: %v", resRead.Err)
	}
	if len(toolCaller.calls) != 1 {
		t.Fatalf("expected read tool to be called in dry run, got calls=%d", len(toolCaller.calls))
	}

	// 2. 写入工具（knowledge_ingest）跳过调用，返回预览
	_ = store.Save(context.Background(), "run-dry-run", "read_step", "ok", pipeline.StepData{
		Items: []map[string]any{{"title": "doc1"}, {"title": "doc2"}},
		Count: 2,
	})
	writeStep := pipeline.StepSpec{
		Key:   "write_step",
		Kind:  "tool",
		Tool:  "knowledge_ingest",
		Needs: []string{"read_step"},
		Args:  json.RawMessage(`{"docs": "{{steps.read_step.items}}"}`),
	}
	resWrite := runner.RunStep(context.Background(), rc, writeStep)
	if resWrite.Status != "ok" {
		t.Fatalf("write step dry run failed: %v", resWrite.Err)
	}
	if len(toolCaller.calls) != 1 { // 调用次数仍为 1，说明未调写入工具
		t.Errorf("write tool should not be called in dry run, calls=%d", len(toolCaller.calls))
	}
	if resWrite.Preview != "dry-run：将写入 2 条" {
		t.Errorf("unexpected write preview: %s", resWrite.Preview)
	}

	// 3. LLM 步骤跳过调用，返回估算字符数与 tokens
	llmStep := pipeline.StepSpec{
		Key:    "llm_step",
		Kind:   "llm",
		Needs:  []string{"read_step"},
		Input:  "read_step",
		Prompt: "整理一下：",
	}
	resLLM := runner.RunStep(context.Background(), rc, llmStep)
	if resLLM.Status != "ok" {
		t.Fatalf("llm step dry run failed: %v", resLLM.Err)
	}
	if len(llmCaller.requests) != 0 {
		t.Errorf("llm should not be called in dry run, requests=%d", len(llmCaller.requests))
	}
	if !strings.Contains(resLLM.Preview, "dry-run：estimated_input_chars=") || !strings.Contains(resLLM.Preview, "estimated_tokens=") {
		t.Errorf("unexpected llm dry run preview: %s", resLLM.Preview)
	}
}

// TestRunner_LLMInputTruncation 测试 LLM 输入数据超限时的截断提示与长字段截断。
func TestRunner_LLMInputTruncation(t *testing.T) {
	store := newFakeDataStore()
	llmCaller := &fakeLLMCaller{
		handler: func(ctx context.Context, req pipeline.LLMRequest) (string, error) {
			if !strings.Contains(req.User, "（另有 1 条未列出）") {
				t.Errorf("expected truncation note in user prompt, got: %s", req.User)
			}
			return "summary", nil
		},
	}

	runner := &pipeline.Runner{
		LLM:  llmCaller,
		Data: store,
	}
	rc := pipeline.RunContext{RunID: "run-trunc-test"}

	// 构造两条 items，第一条很大，使得第二条被截断
	longExcerpt := strings.Repeat("A", 400) // 超出单条 300 rune
	items := []map[string]any{
		{"title": "Item 1", "url": "https://example.com/1", "excerpt": longExcerpt},
		{"title": "Item 2", "url": "https://example.com/2", "excerpt": "Short excerpt"},
	}
	_ = store.Save(context.Background(), "run-trunc-test", "input_data", "ok", pipeline.StepData{
		Items: items,
		Count: 2,
	})

	s := pipeline.StepSpec{
		Key:           "llm_step",
		Kind:          "llm",
		Needs:         []string{"input_data"},
		Input:         "input_data",
		Prompt:        "总结：",
		MaxInputChars: 350, // 设定较小限制迫使截断第二条
	}

	res := runner.RunStep(context.Background(), rc, s)
	if res.Status != "ok" || res.Err != nil {
		t.Fatalf("llm step failed: %v", res.Err)
	}
}

// TestRunner_LLMJSONOutputParsing 测试 LLM 输出 JSON 格式解析。
func TestRunner_LLMJSONOutputParsing(t *testing.T) {
	store := newFakeDataStore()

	// 1. 正常 JSON 数组
	llmCaller := &fakeLLMCaller{
		handler: func(ctx context.Context, req pipeline.LLMRequest) (string, error) {
			return "```json\n[{\"title\": \"A\", \"score\": 95}, {\"title\": \"B\", \"score\": 88}]\n```", nil
		},
	}
	runner := &pipeline.Runner{LLM: llmCaller, Data: store}
	rc := pipeline.RunContext{RunID: "run-json-1"}
	_ = store.Save(context.Background(), "run-json-1", "in", "ok", pipeline.StepData{Count: 1})

	s1 := pipeline.StepSpec{
		Key:    "llm_json",
		Kind:   "llm",
		Needs:  []string{"in"},
		Input:  "in",
		Output: "json",
	}
	res1 := runner.RunStep(context.Background(), rc, s1)
	if res1.Status != "ok" {
		t.Fatalf("step failed: %v", res1.Err)
	}
	if res1.Data.Count != 2 || len(res1.Data.Items) != 2 {
		t.Errorf("expected 2 items parsed from json array, got count=%d", res1.Data.Count)
	}

	// 2. 顶层对象含 items 数组
	llmCaller.handler = func(ctx context.Context, req pipeline.LLMRequest) (string, error) {
		return `{"items": [{"title": "C"}]}`, nil
	}
	rc2 := pipeline.RunContext{RunID: "run-json-2"}
	_ = store.Save(context.Background(), "run-json-2", "in", "ok", pipeline.StepData{Count: 1})
	res2 := runner.RunStep(context.Background(), rc2, s1)
	if res2.Status != "ok" || res2.Data.Count != 1 {
		t.Errorf("expected 1 item parsed from object with items, got count=%d", res2.Data.Count)
	}

	// 3. 非法 JSON：status 仍为 ok，Text 保留，preview 标注 json parse failed
	llmCaller.handler = func(ctx context.Context, req pipeline.LLMRequest) (string, error) {
		return "Sorry, I cannot produce json", nil
	}
	rc3 := pipeline.RunContext{RunID: "run-json-3"}
	_ = store.Save(context.Background(), "run-json-3", "in", "ok", pipeline.StepData{Count: 1})
	res3 := runner.RunStep(context.Background(), rc3, s1)
	if res3.Status != "ok" {
		t.Fatalf("invalid json should still have status ok, got %s", res3.Status)
	}
	if !strings.Contains(res3.Preview, "json parse failed") {
		t.Errorf("expected preview to note json parse failed, got: %s", res3.Preview)
	}
}

// TestRunner_AgentStepRejection 测试 agent 步骤被 Runner 正确拦截。
func TestRunner_AgentStepRejection(t *testing.T) {
	runner := &pipeline.Runner{}
	res := runner.RunStep(context.Background(), pipeline.RunContext{}, pipeline.StepSpec{
		Key:  "agent_step",
		Kind: "agent",
	})
	if res.Status != "error" {
		t.Errorf("expected status=error for agent step, got %s", res.Status)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "agent step is executed by workflowadmin") {
		t.Errorf("unexpected error message: %v", res.Err)
	}
}

// TestRunner_ErrorCases 测试各种错误边界情况。
func TestRunner_ErrorCases(t *testing.T) {
	store := newFakeDataStore()
	runner := &pipeline.Runner{Data: store}
	rc := pipeline.RunContext{RunID: "run-err"}

	// 1. 缺失依赖步骤
	s1 := pipeline.StepSpec{
		Key:   "step_missing_dep",
		Kind:  "transform",
		Needs: []string{"non_existent"},
	}
	res1 := runner.RunStep(context.Background(), rc, s1)
	if res1.Status != "error" || !strings.Contains(res1.Err.Error(), "not found") {
		t.Errorf("expected not found error, got: %v", res1.Err)
	}

	// 2. 上游依赖状态为 error
	_ = store.Save(context.Background(), "run-err", "failed_dep", "error", pipeline.StepData{})
	s2 := pipeline.StepSpec{
		Key:   "step_failed_dep",
		Kind:  "transform",
		Needs: []string{"failed_dep"},
	}
	res2 := runner.RunStep(context.Background(), rc, s2)
	if res2.Status != "error" || !strings.Contains(res2.Err.Error(), "failed with status") {
		t.Errorf("expected dependency failure error, got: %v", res2.Err)
	}

	// 3. 工具执行报错
	failingTool := &fakeToolCaller{
		handler: func(ctx context.Context, name string, args []byte, highTaint bool) ([]byte, error) {
			return nil, apperr.New(apperr.CodeInternal, "network timeout")
		},
	}
	runner.Tools = failingTool
	s3 := pipeline.StepSpec{
		Key:  "failing_tool",
		Kind: "tool",
		Tool: "web_fetch",
		Args: json.RawMessage(`{}`),
	}
	res3 := runner.RunStep(context.Background(), rc, s3)
	if res3.Status != "error" || !strings.Contains(res3.Err.Error(), "network timeout") {
		t.Errorf("expected network timeout error, got: %v", res3.Err)
	}

	// 4. LLM 执行报错
	failingLLM := &fakeLLMCaller{
		handler: func(ctx context.Context, req pipeline.LLMRequest) (string, error) {
			return "", apperr.New(apperr.CodeResourceExhausted, "rate limited")
		},
	}
	runner.LLM = failingLLM
	_ = store.Save(context.Background(), "run-err", "valid_in", "ok", pipeline.StepData{Count: 1})
	s4 := pipeline.StepSpec{
		Key:    "failing_llm",
		Kind:   "llm",
		Needs:  []string{"valid_in"},
		Input:  "valid_in",
		Prompt: "test",
	}
	res4 := runner.RunStep(context.Background(), rc, s4)
	if res4.Status != "error" || !strings.Contains(res4.Err.Error(), "rate limited") {
		t.Errorf("expected rate limited error, got: %v", res4.Err)
	}

	// 5. 未知步骤类型
	s5 := pipeline.StepSpec{
		Key:  "unknown_step",
		Kind: "magic_wand",
	}
	res5 := runner.RunStep(context.Background(), rc, s5)
	if res5.Status != "error" || !strings.Contains(res5.Err.Error(), "unknown step kind") {
		t.Errorf("expected unknown step kind error, got: %v", res5.Err)
	}

	// 6. DataStore 为 nil 时的容错
	nilDataRunner := &pipeline.Runner{}
	s6 := pipeline.StepSpec{
		Key:   "needs_data",
		Kind:  "transform",
		Needs: []string{"upstream"},
	}
	res6 := nilDataRunner.RunStep(context.Background(), rc, s6)
	if res6.Status != "error" || !strings.Contains(res6.Err.Error(), "data store is nil") {
		t.Errorf("expected data store nil error, got: %v", res6.Err)
	}
}

// TestRunner_RunContextVariables 测试工具参数中插值 run.date 与 run.since。
func TestRunner_RunContextVariables(t *testing.T) {
	store := newFakeDataStore()
	toolCaller := &fakeToolCaller{}
	fixedTime := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	runner := &pipeline.Runner{
		Tools: toolCaller,
		Data:  store,
		Now:   func() time.Time { return fixedTime },
	}
	rc := pipeline.RunContext{
		RunID: "run-vars-1",
		Date:  "2026-10-14",
		Since: "2026-10-13T00:00:00Z",
	}

	step := pipeline.StepSpec{
		Key:  "fetch_today",
		Kind: "tool",
		Tool: "web_search",
		Args: json.RawMessage(`{"query": "AI news {{run.date}}", "since": "{{run.since}}"}`),
	}
	res := runner.RunStep(context.Background(), rc, step)
	if res.Status != "ok" {
		t.Fatalf("step failed: %v", res.Err)
	}
	if len(toolCaller.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(toolCaller.calls))
	}
	args := toolCaller.calls[0].args
	if !strings.Contains(args, "2026-10-14") || !strings.Contains(args, "2026-10-13T00:00:00Z") {
		t.Errorf("expected run variables rendered, got: %s", args)
	}
}

// TestRunner_TextOnlyInputAndPreview 测试上游仅有 Text（如 agent 步骤产物）输入至 LLM 以及 Text 摘要预览。
func TestRunner_TextOnlyInputAndPreview(t *testing.T) {
	store := newFakeDataStore()
	llmCaller := &fakeLLMCaller{
		handler: func(ctx context.Context, req pipeline.LLMRequest) (string, error) {
			if !strings.Contains(req.User, "上游 Agent 纯文本分析报告") {
				t.Errorf("expected text in user prompt, got: %s", req.User)
			}
			return "最终简讯内容", nil
		},
	}
	runner := &pipeline.Runner{
		LLM:  llmCaller,
		Data: store,
	}
	rc := pipeline.RunContext{RunID: "run-text-in"}

	// 预设上游步骤产物为纯文本
	_ = store.Save(context.Background(), "run-text-in", "agent_upstream", "ok", pipeline.StepData{
		Text:  "上游 Agent 纯文本分析报告",
		Count: 1,
	})

	step := pipeline.StepSpec{
		Key:    "llm_digest",
		Kind:   "llm",
		Needs:  []string{"agent_upstream"},
		Input:  "agent_upstream",
		Prompt: "整理分析：",
	}
	res := runner.RunStep(context.Background(), rc, step)
	if res.Status != "ok" {
		t.Fatalf("step failed: %v", res.Err)
	}
	if !strings.Contains(res.Preview, "count=1; 最终简讯内容") {
		t.Errorf("unexpected preview for text result: %s", res.Preview)
	}
}

// TestRunner_ItemWithoutTitleOrURLPreview 测试条目没有 title/url 时回退使用首字段展示。
func TestRunner_ItemWithoutTitleOrURLPreview(t *testing.T) {
	store := newFakeDataStore()
	toolCaller := &fakeToolCaller{
		handler: func(ctx context.Context, name string, args []byte, highTaint bool) ([]byte, error) {
			return []byte(`{"items": [{"metric": "cpu_usage", "val": 88}]}`), nil
		},
	}
	runner := &pipeline.Runner{
		Tools: toolCaller,
		Data:  store,
	}
	rc := pipeline.RunContext{RunID: "run-fallback-prev"}

	step := pipeline.StepSpec{
		Key:  "metrics_step",
		Kind: "tool",
		Tool: "feed_fetch",
		Args: json.RawMessage(`{}`),
	}
	res := runner.RunStep(context.Background(), rc, step)
	if res.Status != "ok" {
		t.Fatalf("step failed: %v", res.Err)
	}
	if !strings.Contains(res.Preview, "count=1") {
		t.Errorf("expected count=1 in preview, got: %s", res.Preview)
	}
}

type fakeApprover struct {
	err  error
	reqs []pipeline.ApprovalRequest
}

func (a *fakeApprover) Approve(_ context.Context, req pipeline.ApprovalRequest) error {
	a.reqs = append(a.reqs, req)
	return a.err
}

// TestRunner_HITLApproval 写工具 + approval=hitl 必须先过审批，fail-closed。
func TestRunner_HITLApproval(t *testing.T) {
	deps := &fakeValidateDeps{metas: map[string]pipeline.ToolMeta{
		"post_tool": {Name: "post_tool", Exists: true},
		"read_tool": {Name: "read_tool", ReadOnly: true, Exists: true},
	}}
	step := func(tool string) pipeline.StepSpec {
		return pipeline.StepSpec{Key: "s", Kind: "tool", Tool: tool, Approval: "hitl", Args: json.RawMessage(`{"text":"hi"}`)}
	}
	run := func(ap pipeline.Approver, tool string) (pipeline.StepResult, *fakeToolCaller) {
		tc := &fakeToolCaller{}
		r := &pipeline.Runner{Tools: tc, Data: newFakeDataStore(), Deps: deps, Approver: ap}
		return r.RunStep(context.Background(), pipeline.RunContext{RunID: "r", WorkflowID: "w"}, step(tool)), tc
	}

	t.Run("rejected", func(t *testing.T) {
		ap := &fakeApprover{err: apperr.New(apperr.CodeForbidden, "no")}
		res, tc := run(ap, "post_tool")
		if res.Status != "error" || len(tc.calls) != 0 {
			t.Fatalf("status=%s calls=%d", res.Status, len(tc.calls))
		}
		if len(ap.reqs) != 1 || ap.reqs[0].Tool != "post_tool" || ap.reqs[0].WorkflowID != "w" || ap.reqs[0].StepKey != "s" {
			t.Fatalf("bad request: %+v", ap.reqs)
		}
	})
	t.Run("nil approver", func(t *testing.T) {
		res, tc := run(nil, "post_tool")
		if res.Status != "error" || len(tc.calls) != 0 {
			t.Fatalf("status=%s calls=%d", res.Status, len(tc.calls))
		}
	})
	t.Run("approved", func(t *testing.T) {
		res, tc := run(&fakeApprover{}, "post_tool")
		if res.Status != "ok" || len(tc.calls) != 1 {
			t.Fatalf("status=%s calls=%d err=%v", res.Status, len(tc.calls), res.Err)
		}
	})
	t.Run("read only skips approval", func(t *testing.T) {
		ap := &fakeApprover{err: apperr.New(apperr.CodeForbidden, "no")}
		res, tc := run(ap, "read_tool")
		if res.Status != "ok" || len(tc.calls) != 1 || len(ap.reqs) != 0 {
			t.Fatalf("status=%s calls=%d reqs=%d", res.Status, len(tc.calls), len(ap.reqs))
		}
	})
}

// 写工具缺少 approval 声明（绕过 validate 的数据）时，运行时也必须拒绝且不调用工具。
func TestRunner_WriteToolWithoutApprovalDeclRejected(t *testing.T) {
	deps := &fakeValidateDeps{metas: map[string]pipeline.ToolMeta{"post_tool": {Name: "post_tool", Exists: true}}}
	tc := &fakeToolCaller{}
	ap := &fakeApprover{}
	r := &pipeline.Runner{Tools: tc, Data: newFakeDataStore(), Deps: deps, Approver: ap}
	res := r.RunStep(context.Background(), pipeline.RunContext{RunID: "r"},
		pipeline.StepSpec{Key: "s", Kind: "tool", Tool: "post_tool", Args: json.RawMessage(`{}`)})
	if res.Status != "error" || len(tc.calls) != 0 || len(ap.reqs) != 0 {
		t.Fatalf("status=%s calls=%d approvals=%d", res.Status, len(tc.calls), len(ap.reqs))
	}
	if apperr.CodeOf(res.Err) != apperr.CodeForbidden {
		t.Fatalf("want CodeForbidden, got %v", res.Err)
	}
}
