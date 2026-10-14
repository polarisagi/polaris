package workflowadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// BackgroundAdmitter 后台工作资源准入闸门（消费端接口，签名同 connector.BackgroundAdmitter）。
type BackgroundAdmitter interface {
	AdmitBackground(work string) (release func(), admitted bool)
}

// ResultDeliverer 结果投递接口。
type ResultDeliverer interface {
	DeliverResult(ctx context.Context, resultAction, sessionID, title, text string) error
}

// HITLPrompter 写入类管线工具步骤审批所需的最小 HITL 能力（protocol.HITL 的子集）。
type HITLPrompter interface {
	Prompt(ctx context.Context, p types.HITLPrompt) (*types.HITLResponse, error)
}

// hitlApprover 把 pipeline.Approver 适配到 HITL 网关；沿用 RequestApproval 的
// tool_execution checkpoint 类型，避免新增需要登记超时策略的类型。
type hitlApprover struct {
	gw HITLPrompter
}

func (a hitlApprover) Approve(ctx context.Context, req pipeline.ApprovalRequest) error {
	if a.gw == nil {
		return apperr.New(apperr.CodeForbidden, "hitl gateway is not configured")
	}
	resp, err := a.gw.Prompt(ctx, types.HITLPrompt{
		ID:             "pipeline_tool_step:" + req.RunID + ":" + req.StepKey,
		CheckpointType: "tool_execution",
		PromptText: fmt.Sprintf("Request action: pipeline_tool_step\nworkflow=%s run=%s step=%s tool=%s\nargs=%s",
			req.WorkflowID, req.RunID, req.StepKey, req.Tool, req.ArgsPreview),
		RiskLevel: int(types.RiskHigh),
		Options:   []types.HITLOption{{Key: "approve", Label: "Approve"}, {Key: "deny", Label: "Deny"}},
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeForbidden, "hitl prompt", err)
	}
	if resp == nil || !resp.Approved {
		return apperr.New(apperr.CodeForbidden, "hitl: request rejected")
	}
	return nil
}

// toolCallerAdapter 管线 tool 步骤唯一出口：Dispatcher.ExecuteWithTaint（含审计拦截器、PolicyGate、污点出口），HE-3。
type toolCallerAdapter struct {
	exec protocol.AgentToolExecutor
}

func (a toolCallerAdapter) Call(ctx context.Context, name string, args []byte, high bool) ([]byte, error) {
	if a.exec == nil {
		return nil, apperr.New(apperr.CodeInternal, "tool executor is nil")
	}
	taint := types.TaintLow
	if high {
		taint = types.TaintHigh
	}
	res, err := a.exec.ExecuteWithTaint(protocol.WithBackgroundWork(ctx), name, args, taint)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "execute tool: "+name, err)
	}
	if res == nil {
		return nil, apperr.New(apperr.CodeInternal, "tool execution returned nil result")
	}
	if !res.Success || res.Error != "" {
		errMsg := res.Error
		if errMsg == "" {
			errMsg = string(res.Output)
		}
		if errMsg == "" {
			errMsg = "tool execution failed"
		}
		return nil, apperr.New(apperr.CodeInternal, errMsg)
	}
	return res.Output, nil
}

// llmCallerAdapter 适配 LLMInfer 函数至 pipeline.LLMCaller。
type llmCallerAdapter struct {
	infer func(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error)
}

func (a llmCallerAdapter) Complete(ctx context.Context, req pipeline.LLMRequest) (string, error) {
	if a.infer == nil {
		return "", apperr.New(apperr.CodeInternal, "llm infer function is nil")
	}
	// 透传后台上下文与任务追踪标记
	bgCtx := context.WithValue(protocol.WithBackgroundWork(ctx), protocol.CtxTaskIDKey{}, req.RunID)

	msgs := []types.Message{
		{Role: "system", Content: req.System},
		{Role: "user", Content: req.User},
	}

	opts := []types.InferOption{
		types.WithPurpose(types.PurposeWorkflowLLMStep),
		types.WithModelPool(string(types.ModelPoolDefault)),
	}

	if req.MaxOutputTokens > 0 {
		opts = append(opts, types.WithMaxTokens(req.MaxOutputTokens))
	}

	switch req.Thinking {
	case "low":
		opts = append(opts, types.WithThinkingMode(types.ThinkingLow))
	case "high":
		opts = append(opts, types.WithThinkingMode(types.ThinkingHigh))
	case "max":
		opts = append(opts, types.WithThinkingMode(types.ThinkingMax))
	default:
		opts = append(opts, types.WithThinkingMode(types.ThinkingDisabled))
	}

	if len(req.JSONSchema) > 0 {
		opts = append(opts, types.WithResponseFormat(&types.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: req.JSONSchema,
		}))
	}

	resp, err := a.infer(bgCtx, msgs, opts...)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "infer workflow llm step", err)
	}
	if resp == nil {
		return "", apperr.New(apperr.CodeInternal, "llm response is nil")
	}
	return resp.Content, nil
}

// runDataAdapter 适配 WorkflowRepository 至 pipeline.RunDataStore。
type runDataAdapter struct {
	repo repo.WorkflowRepository
}

func (a runDataAdapter) Save(ctx context.Context, runID, key, status string, d pipeline.StepData) error {
	if a.repo == nil {
		return apperr.New(apperr.CodeInternal, "workflow repository is nil")
	}
	b, err := json.Marshal(d)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "marshal step data", err)
	}
	if err := a.repo.SaveRunData(ctx, runID, key, status, b, d.Count); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "save run data", err)
	}
	return nil
}

func (a runDataAdapter) Load(ctx context.Context, runID, key string) (status string, d pipeline.StepData, ok bool, err error) {
	if a.repo == nil {
		return "", pipeline.StepData{}, false, apperr.New(apperr.CodeInternal, "workflow repository is nil")
	}
	st, dataJSON, err := a.repo.GetRunData(ctx, runID, key)
	if err != nil {
		return "", pipeline.StepData{}, false, apperr.Wrap(apperr.CodeInternal, "load run data", err)
	}
	if st == "" && len(dataJSON) == 0 {
		return "", pipeline.StepData{}, false, nil
	}
	var stepData pipeline.StepData
	if len(dataJSON) > 0 {
		if err := json.Unmarshal(dataJSON, &stepData); err != nil {
			//nolint:nilerr // 数据格式非标准 StepData JSON 时作为纯文本降级返回
			return st, pipeline.StepData{Text: string(dataJSON), Count: 1}, true, nil
		}
	}
	return st, stepData, true, nil
}

// dedupState 跨步骤共享去重状态。
type dedupState struct {
	repo    repo.WorkflowRepository
	wfID    string
	mu      sync.Mutex
	pending []string
}

func (s *dedupState) Seen(keys []string) (map[string]bool, error) {
	if s.repo == nil {
		return make(map[string]bool), nil
	}
	seen, err := s.repo.SeenKeys(context.Background(), s.wfID, keys)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "query seen keys", err)
	}
	return seen, nil
}

func (s *dedupState) Pending(keys []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, keys...)
}

func (s *dedupState) Commit(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 || s.repo == nil {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := s.repo.MarkSeen(ctx, s.wfID, s.pending, now); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mark seen keys", err)
	}
	s.pending = nil
	return nil
}

// toolMetaAdapter 适配 AgentToolExecutor 至 pipeline.ValidateDeps。
type toolMetaAdapter struct {
	exec protocol.AgentToolExecutor
	// writeAllowlist 免审批写工具白名单，来自 thresholds.m13_interface.pipeline_write_allowlist（ADR-0114 决策八）。
	writeAllowlist []string
}

// newToolMetaAdapter 构造适配器并从阈值配置读取写入白名单；pipeline 包本身不读 config。
func newToolMetaAdapter(exec protocol.AgentToolExecutor) toolMetaAdapter {
	return toolMetaAdapter{exec: exec, writeAllowlist: config.CurrentThresholds().M13Interface.PipelineWriteAllowlist}
}

func (a toolMetaAdapter) ToolMeta(name string) pipeline.ToolMeta {
	if a.exec == nil {
		return pipeline.ToolMeta{Name: name, Exists: false}
	}
	tool, err := a.exec.Lookup(name)
	if err != nil {
		return pipeline.ToolMeta{Name: name, Exists: false}
	}

	// ReadOnly 判定：
	// MCP 工具在 Capability 上默认为零值 CapReadOnly，因此必须且仅看服务器声明的 ReadOnlyHint，
	// 忽略 Capability 零值（规范明确 annotations 仅为提示，不可用作安全边界，但管线允许集以只读为准）。
	// 非 MCP 内置工具依 Capability 及白名单（web_fetch / feed_fetch）判定。
	var isReadOnly bool
	if tool.Source == types.ToolMCP {
		isReadOnly = tool.ReadOnlyHint
	} else {
		isReadOnly = tool.Capability == types.CapReadOnly || name == "web_fetch" || name == "feed_fetch"
	}
	isWriteAllowed := slices.Contains(a.writeAllowlist, name)
	isPrivileged := tool.Capability >= types.CapPrivileged

	return pipeline.ToolMeta{
		Name:         name,
		ReadOnly:     isReadOnly,
		WriteAllowed: isWriteAllowed,
		Privileged:   isPrivileged,
		Exists:       true,
	}
}
