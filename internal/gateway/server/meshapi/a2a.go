package meshapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/polarisagi/polaris/internal/gateway/mesh"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// a2aHandler 用 mesh.Service 实现 a2asrv.RequestHandler。
//
// 为什么不用 SDK 自带的 a2asrv.NewHandler(executor)：那套执行模型要求执行器在进程内
// 产出事件流、任务存进 SDK 自己的 TaskStore。Polaris 的任务在黑板上（带崩溃恢复、
// 租约、HITL 挂起），由既有 Worker 执行；把执行搬进 SDK 等于维护第二套任务状态机。
// 这里只借 SDK 的类型、JSON-RPC 传输与错误编码（google.rpc.Status + ErrorInfo），
// 语义全部落在 mesh.Service。
type a2aHandler struct {
	svc          *mesh.Service
	blockingWait time.Duration
}

var _ a2asrv.RequestHandler = (*a2aHandler)(nil)

// checkVersion A2A-Version 协商（规范：缺省视为当前版本；不支持的版本返回 VersionNotSupported）。
func checkVersion(ctx context.Context) error {
	cc, ok := a2asrv.CallContextFrom(ctx)
	if !ok {
		return nil
	}
	vals, ok := cc.ServiceParams().Get(a2a.SvcParamVersion)
	if !ok || len(vals) == 0 || vals[0] == "" {
		return nil
	}
	if vals[0] != string(a2a.Version) {
		return a2a.NewError(a2a.ErrVersionNotSupported, fmt.Sprintf("supported A2A version is %s, got %s", a2a.Version, vals[0]))
	}
	return nil
}

func (h *a2aHandler) principal(ctx context.Context) (mesh.Principal, error) {
	if err := checkVersion(ctx); err != nil {
		return mesh.Principal{}, err
	}
	p, ok := principalFrom(ctx)
	if !ok {
		return mesh.Principal{}, a2a.ErrUnauthenticated
	}
	return p, nil
}

// SendMessage 受理一次委派。缺省阻塞等待到终态 / INPUT_REQUIRED / blockingWait 超时；
// configuration.returnImmediately=true 时立即返回 SUBMITTED 任务。
func (h *a2aHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	p, err := h.principal(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil || req.Message == nil {
		return nil, a2a.NewError(a2a.ErrInvalidParams, "message is required")
	}
	msg := req.Message
	if msg.TaskID != "" {
		// 多轮续写同一任务不支持：唯一需要续写的场景是补充输入，而本节点的「待输入」
		// 只可能是本地人类审批（决策五），对端续写恰好是要禁止的那条路。
		return nil, a2a.NewError(a2a.ErrUnsupportedOperation,
			"continuing an existing task is not supported; INPUT_REQUIRED tasks can only be resolved by a local human")
	}
	text, err := extractText(msg.Parts)
	if err != nil {
		return nil, err
	}
	deleg, err := delegationFromMetadata(msg.Metadata)
	if err != nil {
		return nil, err
	}
	t, err := h.svc.Submit(ctx, p, mesh.SubmitRequest{
		Protocol: mesh.ProtocolA2A, ContextID: msg.ContextID, MessageID: msg.ID, Text: text, Delegation: deleg,
	})
	if err != nil {
		return nil, toA2AError(err)
	}
	if t.State.Terminal() || (req.Config != nil && req.Config.ReturnImmediately) {
		return toA2ATask(t, h.svc.Self().NodeID, historyLen(req.Config)), nil
	}
	if t, err = h.svc.Await(ctx, p, t.ID, h.blockingWait); err != nil {
		return nil, toA2AError(err)
	}
	return toA2ATask(t, h.svc.Self().NodeID, historyLen(req.Config)), nil
}

// GetTask 只返回调用方自己的任务。
func (h *a2aHandler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	p, err := h.principal(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.svc.Get(ctx, p, string(req.ID))
	if err != nil {
		return nil, toA2AError(err)
	}
	return toA2ATask(t, h.svc.Self().NodeID, req.HistoryLength), nil
}

// ListTasks 按调用方作用域过滤，游标分页。
func (h *a2aHandler) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	p, err := h.principal(ctx)
	if err != nil {
		return nil, err
	}
	var state mesh.State
	if req.Status != a2a.TaskStateUnspecified {
		if state, err = fromA2AState(req.Status); err != nil {
			return nil, err
		}
	}
	tasks, next, total, err := h.svc.List(ctx, p, mesh.ListFilter{
		ContextID: req.ContextID, State: state, PageSize: req.PageSize, PageToken: req.PageToken,
	})
	if err != nil {
		return nil, toA2AError(err)
	}
	out := &a2a.ListTasksResponse{Tasks: make([]*a2a.Task, 0, len(tasks)), TotalSize: total,
		PageSize: len(tasks), NextPageToken: next}
	for _, t := range tasks {
		at := toA2ATask(t, h.svc.Self().NodeID, req.HistoryLength)
		if !req.IncludeArtifacts {
			at.Artifacts = nil
		}
		out.Tasks = append(out.Tasks, at)
	}
	return out, nil
}

// CancelTask 映射到黑板取消。
func (h *a2aHandler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	p, err := h.principal(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.svc.Cancel(ctx, p, string(req.ID))
	if err != nil {
		return nil, toA2AError(err)
	}
	return toA2ATask(t, h.svc.Self().NodeID, nil), nil
}

// 流式接口第二批再做（capabilities.streaming=false 已在卡片上声明）。
func (h *a2aHandler) SubscribeToTask(context.Context, *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return errSeq(a2a.ErrUnsupportedOperation)
}

func (h *a2aHandler) SendStreamingMessage(context.Context, *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return errSeq(a2a.ErrUnsupportedOperation)
}

// 推送通知本期不做（capabilities.pushNotifications=false）。
func (h *a2aHandler) GetTaskPushConfig(context.Context, *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

func (h *a2aHandler) ListTaskPushConfigs(context.Context, *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

func (h *a2aHandler) CreateTaskPushConfig(context.Context, *a2a.PushConfig) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

func (h *a2aHandler) DeleteTaskPushConfig(context.Context, *a2a.DeleteTaskPushConfigRequest) error {
	return a2a.ErrPushNotificationNotSupported
}

// GetExtendedAgentCard 本期未配置扩展卡（capabilities.extendedAgentCard=false）。
func (h *a2aHandler) GetExtendedAgentCard(context.Context, *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return nil, a2a.ErrExtendedCardNotConfigured
}

func errSeq(err error) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { yield(nil, err) }
}

func historyLen(cfg *a2a.SendMessageConfig) *int {
	if cfg == nil {
		return nil
	}
	return cfg.HistoryLength
}

// extractText 把 Part 列表规整成一段文本。
//   - text：原样拼接；
//   - data：JSON 序列化后拼接（内容随任务整体按 TaintHigh 处理，不做结构化信任）；
//   - url：只记下地址，**不自动抓取**（决策二第 8 条：抓取须经 SafeDialer + 策略许可，交给执行中的 Agent 决定）；
//   - raw：二进制内容不支持，按 CONTENT_TYPE_NOT_SUPPORTED 拒绝。
func extractText(parts a2a.ContentParts) (string, error) {
	var b strings.Builder
	for _, part := range parts {
		if part == nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		switch c := part.Content.(type) {
		case a2a.Text:
			b.WriteString(string(c))
		case a2a.Data:
			raw, err := json.Marshal(c.Value)
			if err != nil {
				return "", a2a.NewError(a2a.ErrInvalidParams, "data part is not JSON-serializable")
			}
			b.Write(raw)
		case a2a.URL:
			fmt.Fprintf(&b, "[attached URL, not fetched: %s]", string(c))
		default:
			return "", a2a.NewError(a2a.ErrUnsupportedContentType, "only text, data and url parts are supported")
		}
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", a2a.NewError(a2a.ErrInvalidParams, "message has no text content")
	}
	return b.String(), nil
}

// delegationFromMetadata 读取 Polaris 委派扩展。缺失返回 nil（按外部调用方处理）；格式错误返回 InvalidParams。
func delegationFromMetadata(md map[string]any) (*mesh.Delegation, error) {
	v, ok := md[mesh.DelegationExtensionURI]
	if !ok {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, a2a.NewError(a2a.ErrInvalidParams, "delegation metadata is not an object")
	}
	var d mesh.Delegation
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, a2a.NewError(a2a.ErrInvalidParams, "delegation metadata must be {spawn_depth:int, chain:[string]}")
	}
	return &d, nil
}

// toA2AState / fromA2AState 状态映射（ADR-0111 决策二第 4 条）。
func toA2AState(s mesh.State) a2a.TaskState {
	switch s {
	case mesh.StateSubmitted:
		return a2a.TaskStateSubmitted
	case mesh.StateWorking:
		return a2a.TaskStateWorking
	case mesh.StateInputRequired:
		return a2a.TaskStateInputRequired
	case mesh.StateCompleted:
		return a2a.TaskStateCompleted
	case mesh.StateFailed:
		return a2a.TaskStateFailed
	case mesh.StateCanceled:
		return a2a.TaskStateCanceled
	case mesh.StateRejected:
		return a2a.TaskStateRejected
	default:
		return a2a.TaskStateUnspecified
	}
}

func fromA2AState(s a2a.TaskState) (mesh.State, error) {
	all := []mesh.State{mesh.StateSubmitted, mesh.StateWorking, mesh.StateInputRequired, mesh.StateCompleted,
		mesh.StateFailed, mesh.StateCanceled, mesh.StateRejected}
	idx := slices.IndexFunc(all, func(m mesh.State) bool { return toA2AState(m) == s })
	if idx < 0 {
		return "", a2a.NewError(a2a.ErrInvalidParams, "unsupported task state filter "+string(s))
	}
	return all[idx], nil
}

// toA2ATask 委派任务 → A2A Task。结果放在名为 result 的 Artifact 里；状态说明放在
// status.message（ROLE_AGENT）。时间戳 UTC 毫秒精度。
func toA2ATask(t *mesh.Task, selfNodeID string, historyLength *int) *a2a.Task {
	ts := t.UpdatedAt.UTC().Truncate(time.Millisecond)
	out := &a2a.Task{
		ID:        a2a.TaskID(t.ID),
		ContextID: t.ContextID,
		Status:    a2a.TaskStatus{State: toA2AState(t.State), Timestamp: &ts},
		Metadata:  map[string]any{"polaris.node_id": selfNodeID},
	}
	if t.StatusMessage != "" {
		out.Status.Message = &a2a.Message{ID: t.ID + "-status", ContextID: t.ContextID, TaskID: a2a.TaskID(t.ID),
			Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{a2a.NewTextPart(t.StatusMessage)}}
	}
	if historyLength == nil || *historyLength > 0 {
		out.History = []*a2a.Message{{ID: firstNonEmpty(t.MessageID, t.ID+"-input"), ContextID: t.ContextID,
			TaskID: a2a.TaskID(t.ID), Role: a2a.MessageRoleUser, Parts: a2a.ContentParts{a2a.NewTextPart(t.Input)}}}
	}
	if t.State == mesh.StateCompleted {
		out.Artifacts = []*a2a.Artifact{{ID: "result", Name: "result", Parts: a2a.ContentParts{a2a.NewTextPart(t.Result)}}}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// toA2AError apperr → A2A 错误（google.rpc.Status reason 由 SDK 依错误类型编码）。
func toA2AError(err error) error {
	var ae *a2a.Error
	if errors.As(err, &ae) {
		return err
	}
	switch apperr.CodeOf(err) {
	case apperr.CodeNotFound:
		return a2a.NewError(a2a.ErrTaskNotFound, "task not found")
	case apperr.CodeConflict:
		return a2a.NewError(a2a.ErrTaskNotCancelable, "task is already in a terminal state")
	case apperr.CodeForbidden:
		return a2a.NewError(a2a.ErrUnauthorized, err.Error())
	case apperr.CodeUnauthorized:
		return a2a.ErrUnauthenticated
	case apperr.CodeInvalidInput:
		return a2a.NewError(a2a.ErrInvalidParams, err.Error())
	default:
		return a2a.NewError(a2a.ErrInternalError, "internal error")
	}
}
