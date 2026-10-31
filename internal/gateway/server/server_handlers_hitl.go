package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin"
	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// handleGetPendingApprovals/handleAgentInterrupt/handleResolveApproval（HITL
// 审批 + 中断处理）、agentStateString/handleStatus（WebUI 状态快照）见本文件
// （R7 拆分自 server_handlers.go；健康检查/配置/评测/Agent 查询任务提交见
// server_handlers.go）。

// handleGetPendingApprovals 获取待审批任务。
func (s *Server) handleGetPendingApprovals(w http.ResponseWriter, r *http.Request) {
	if s.hitlGateway == nil {
		httputil.RespondError(w, "Approvals are unavailable", nil, http.StatusNotImplemented)
		return
	}

	pending, err := s.hitlGateway.Pending(r.Context())
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"pending": pending,
	})
}

// parseInterruptAction 将请求体里的字符串动作解析为 types.InterruptAction 枚举。
func parseInterruptAction(action string) types.InterruptAction {
	switch action {
	case "resume":
		return types.InterruptResume
	case "redirect":
		return types.InterruptRedirect
	case "abort":
		return types.InterruptAbort
	default:
		return types.InterruptResume
	}
}

// dispatchInterruptRequest 处理用户中断请求（M13 §1.2.5，inv_global_08 <200ms SLO）。
// POST /v1/agent/{taskID}/interrupt
// body: {"action":"resume"|"redirect"|"abort","redirect":"新意图文本","reason":"..."}
// dispatchInterruptRequest 将中断请求路由到目标 Agent：优先经 Outbox 异步分发，
// 无 outboxWriter 时降级为经 AgentPool 直接调用（从 handleAgentInterrupt 拆出，
// nestif 治理，行为不变）。
func (s *Server) dispatchInterruptRequest(ctx context.Context, taskID, action string, interruptReq types.InterruptRequest) {
	if s.outboxWriter != nil {
		// 异步路由：写入 Outbox，由 OutboxWorker 分发到目标 Agent 进程。
		// OutboxWorker 需注册 operation="agent_interrupt" 的处理器（见 internal/store/outbox_worker.go）。
		//
		// 注释与实现对齐修复：此前日志写着"falling back to direct call"，但紧跟
		// 一个无条件 return——异步投递失败时中断请求实际被静默丢弃，用户点击
		// "中止/恢复/重定向"毫无反应，正是本端点 2026-07-12 修过一次的同类缺口。
		// 现改为构造/投递任一失败都真正落到下方的 AgentPool 直接调用路径。
		ev, evErr := protocol.NewOutboxEvent(protocol.TopicAgentInterrupt, "agent_interrupt", map[string]any{
			"task_id": taskID,
			"request": interruptReq,
		}, string(types.BuildIdempotencyKey(protocol.TopicAgentInterrupt, "task", taskID, action, 1)))
		switch {
		case evErr != nil:
			slog.Error("handleAgentInterrupt: build outbox event failed, falling back to direct call",
				"task_id", taskID, "err", evErr)
		default:
			ev.Scope = taskID
			if err := s.outboxWriter.Write(ctx, ev); err != nil {
				slog.Error("handleAgentInterrupt: outbox write failed, falling back to direct call",
					"task_id", taskID, "err", err)
				break
			}
			return
		}
	}

	if s.agentPool == nil {
		slog.Warn("handleAgentInterrupt: neither outboxWriter nor agentPool available, interrupt request dropped", "task_id", taskID)
		return
	}

	// 单进程降级路径修复（2026-07-12）：此前该分支的注释声称"直接调用"，
	// 实际实现只打一行 INFO 日志——outboxWriter 此前从未在 cmd/polaris 启动
	// 流程中被注入（SetOutboxWriter 从未被调用，见 server_core.go 修复），
	// 意味着本端点在生产环境下恒定静默丢弃中断请求，客户端点击"中止/恢复/
	// 重定向"实际上什么都不会发生。现在真正实现直接调用：与 sse.go
	// 交互式编排的客户端断连分支（ctx.Done() → agentCtrl.Interrupt）
	// 使用同一条路径——经 AgentPool.Acquire 取得目标会话的 AgentController
	// 后直接调用 Interrupt，用完立即 release。
	agentCtrl, release, acqErr := s.agentPool.Acquire(ctx, taskID)
	if acqErr != nil {
		slog.Error("handleAgentInterrupt: direct-call fallback failed to acquire agent controller", "task_id", taskID, "err", acqErr)
		return
	}
	agentCtrl.Interrupt(interruptReq)
	release()
}

// interruptDebounceWindow 单 task 中断防抖窗口（M13 §1.2.5）。
const interruptDebounceWindow = 30 * time.Second

// authContextOrLocalWebUI D-1: 本地 WebUI 访问在零认证配置下 authCtx 可能为 nil，
// 直接访问字段会 panic（GR-9.1-005），此时按未认证的本地 WebUI 处理。
func authContextOrLocalWebUI(r *http.Request) *authcontext.AuthContext {
	if authCtx := authcontext.FromContext(r.Context()); authCtx != nil {
		return authCtx
	}
	return &authcontext.AuthContext{ClientType: authcontext.ClientTypeLocalWebUI}
}

func (s *Server) handleAgentInterrupt(w http.ResponseWriter, r *http.Request) {
	clientIP := extractIP(r)
	authCtx := authContextOrLocalWebUI(r)
	clientType := authCtx.ClientType

	// GR-9.1-001：限流键与全局中间件同一命名空间（clientIP:clientType，M13 §1.5）；
	// 裸 IP 会让同 IP 不同 clientType 共用首个访问者配额创建的桶。
	if s.interruptLimiter != nil && !s.interruptLimiter.Allow(clientIP+":"+string(clientType), string(clientType)) {
		httputil.RespondError(w, "Rate limit exceeded", nil, http.StatusTooManyRequests)
		return
	}

	taskID := r.PathValue("taskID")

	isAdmin := authCtx.Authenticated && (authCtx.UserID == "admin" || authCtx.UserID == "system")
	isLocalWebUI := authCtx.ClientType.IsLocalTrusted()
	if !isAdmin && !isLocalWebUI {
		httputil.RespondError(w, "Forbidden", nil, http.StatusForbidden)
		return
	}

	// M13 §1.2.5 单 task 防抖：同一 task 30s 内重复中断 → 429。按 task 计而非按
	// 客户端计——多个客户端对同一任务连点中断同样会造成状态机抖动。
	if _, dup := s.interruptDebounce.LoadOrStore(taskID, struct{}{}); dup {
		httputil.RespondError(w, "Task interruption is already in progress", nil, http.StatusTooManyRequests)
		return
	}
	time.AfterFunc(interruptDebounceWindow, func() { s.interruptDebounce.Delete(taskID) })

	var req struct {
		Action   string `json:"action"`   // "resume" | "redirect" | "abort"
		Redirect string `json:"redirect"` // action=redirect 时的新意图
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}

	action := parseInterruptAction(req.Action)

	interruptReq := types.InterruptRequest{
		Reason:   req.Reason,
		Action:   action,
		Redirect: req.Redirect,
		// 信任来自已认证的本地可信客户端，而非请求体自报。
		OwnerTrusted: authCtx.Authenticated && authCtx.ClientType.IsLocalTrusted(),
	}
	s.dispatchInterruptRequest(r.Context(), taskID, req.Action, interruptReq)
	if s.hooks != nil {
		hooks, reason := s.hooks, req.Reason
		if reason == "" {
			reason = req.Action
		}
		concurrent.SafeGo(context.WithoutCancel(r.Context()), "gateway.interrupt_hook", func(ctx context.Context) {
			hooks.Interrupt(ctx, taskID, reason)
		})
	}

	if s.auditTrail != nil {
		detail, err := json.Marshal(map[string]any{
			"task_id":  taskID,
			"action":   req.Action,
			"redirect": req.Redirect,
			"reason":   req.Reason,
		})
		if err != nil {
			slog.Error("handleAgentInterrupt: failed to marshal audit detail", "err", err)
			detail = []byte(`{"error":"marshal_failed"}`)
		}
		if err := s.auditTrail.Record(&security.AuditRecord{
			ActionType:   "interrupt",
			ActionDetail: detail,
			AgentID:      authCtx.UserID,
			Timestamp:    time.Now().UnixMicro(),
		}); err != nil {
			// 审计写入失败不阻断中断请求主流程（用户中断意图已通过上方 outbox/直调路径生效），
			// 但必须留痕，避免"审计系统失效却无人发现"（同 doc03 §3.2 LogAudit 原则）。
			slog.Error("handleAgentInterrupt: audit record failed", "task_id", taskID, "err", err)
		}
	}

	httputil.WriteJSON(w, map[string]string{
		"status": "accepted",
		"taskID": taskID,
	})
}

// handleResolveApproval 提交审批结果。
func (s *Server) handleResolveApproval(w http.ResponseWriter, r *http.Request) {
	if s.hitlGateway == nil {
		httputil.RespondError(w, "Approvals are unavailable", nil, http.StatusNotImplemented)
		return
	}

	pathParts := strings.Split(r.URL.Path, "/")
	if len(pathParts) < 5 || pathParts[4] != "resolve" {
		httputil.RespondError(w, "Invalid path", nil, http.StatusBadRequest)
		return
	}
	approvalID := pathParts[3]

	var req struct {
		Action  string `json:"action"` // "approve" or "deny"
		Comment string `json:"comment"`
		// Remember 对应审批卡的“以后不再询问”勾选框；仅在批准时有意义。
		Remember bool `json:"remember"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}

	authCtx := authcontext.FromContext(r.Context())

	resp := types.HITLResponse{
		OptionKey: req.Action,
		Approved:  req.Action == "approve",
		Reason:    req.Comment,
		UserID:    authCtx.UserID, // M13: 接入鉴权上下文
		Remember:  req.Remember && req.Action == "approve",
	}

	err := s.hitlGateway.Respond(r.Context(), approvalID, resp)
	if err != nil {
		// 状态码跟随错误码：不存在 404、已决/已超时 409、前置校验不过 403，其余才是 500。
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}

	httputil.WriteJSON(w, map[string]string{"status": "ok"})
}

// handleStatus 返回 WebUI statusBar 所需的运行时指标快照。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	memMB := memStats.Sys / (1024 * 1024)

	// 从 registry 取当前对话模型名称
	modelID := s.registry.PickProviderName("default")
	if modelID == "" {
		modelID = s.registry.PickProviderName("general")
	}

	// Agent state
	agentState := ""
	agentID := ""
	agentConfig := map[string]any{}
	// Global agent status removed

	// KillFullStop = 3；PolarisKillswitchStage 由 main.go KillSwitch 回调写入
	sealed := metrics.GlobalKillswitchStage.Load() >= 3

	// 用量：?session_id= 给该会话（含上下文占用），缺省给今日全局累计。此前 token_used/token_limit/
	// cost_cny 写死为 0，状态栏恒显示 0。币种为 USD，折算由客户端按 fx_cny_per_usd 展示。
	usage := s.statusUsage(r)

	httputil.WriteJSON(w, map[string]any{
		"sealed":          sealed,
		"model_id":        modelID,
		"usage":           usage,
		"fx_cny_per_usd":  config.UsageDisplayFXCNYPerUSD,
		"memory_mb":       memMB,
		"memory_limit_mb": 8192,
		"agent_id":        agentID,
		"agent_state":     agentState,
		"agent_config":    agentConfig,
		"embedding":       s.GetEmbeddingStatus(),
		"accelerator":     s.GetAcceleratorInfo(),
	})
}

// statusUsage 组装 /v1/status 的 usage 字段。查询失败不让状态快照整体失败（状态栏其余指标仍有用），
// 以 error 字段如实标出，客户端据此显示"用量不可用"而不是 0。
func (s *Server) statusUsage(r *http.Request) map[string]any {
	sid := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if sid != "" && !session.SessionIDPattern.MatchString(sid) {
		return map[string]any{"error": "invalid session_id"}
	}
	if s.sysadminHandler == nil || s.sysadminHandler.UsageRepo == nil {
		return map[string]any{"error": "usage ledger unavailable"}
	}
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	view, err := sysadmin.BuildSessionUsage(r.Context(), s.sysadminHandler.UsageRepo, sid, dayStart)
	if err != nil {
		slog.Warn("handleStatus: usage query failed", "session", sid, "err", err)
		return map[string]any{"error": "usage query failed"}
	}
	scope := "today"
	if sid != "" {
		scope = "session"
	}
	return map[string]any{"scope": scope, "data": view}
}
