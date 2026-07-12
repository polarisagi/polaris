package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security"
	"github.com/polarisagi/polaris/pkg/types"
)

// handleGetPendingApprovals/handleAgentInterrupt/handleResolveApproval（HITL
// 审批 + 中断处理）、agentStateString/handleStatus（WebUI 状态快照）见本文件
// （R7 拆分自 server_handlers.go；健康检查/配置/评测/Agent 查询任务提交见
// server_handlers.go）。

// handleGetPendingApprovals 获取待审批任务。
func (s *Server) handleGetPendingApprovals(w http.ResponseWriter, r *http.Request) {
	if s.hitlGateway == nil {
		http.Error(w, "HITL not enabled", http.StatusNotImplemented)
		return
	}

	pending, err := s.hitlGateway.Pending(r.Context())
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
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

// handleAgentInterrupt 处理用户中断请求（M13 §1.2.5，inv_global_08 <200ms SLO）。
// POST /v1/agent/{taskID}/interrupt
// body: {"action":"resume"|"redirect"|"abort","redirect":"新意图文本","reason":"..."}
func (s *Server) handleAgentInterrupt(w http.ResponseWriter, r *http.Request) {
	clientIP := extractIP(r)
	authCtx := authcontext.FromContext(r.Context())
	clientType := "unknown"
	if authCtx != nil {
		clientType = authCtx.ClientType
	}
	if s.interruptLimiter != nil && !s.interruptLimiter.Allow(clientIP, clientType) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	taskID := r.PathValue("taskID")

	if authCtx == nil || (authCtx.UserID != "admin" && authCtx.UserID != "system") {
		// MVP 阶段仅 admin 可操作。在多租户下需检查 task 所属 user。
		http.Error(w, "forbidden: unauthorized user", http.StatusForbidden)
		return
	}

	var req struct {
		Action   string `json:"action"`   // "resume" | "redirect" | "abort"
		Redirect string `json:"redirect"` // action=redirect 时的新意图
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}

	action := parseInterruptAction(req.Action)

	interruptReq := types.InterruptRequest{
		Reason:   req.Reason,
		Action:   action,
		Redirect: req.Redirect,
	}
	if s.outboxWriter != nil {
		// 异步路由：写入 Outbox，由 OutboxWorker 分发到目标 Agent 进程。
		// OutboxWorker 需注册 operation="agent_interrupt" 的处理器（见 pkg/substrate/storage/outbox_worker.go）。
		ev, _ := protocol.NewOutboxEvent(protocol.TopicAgentInterrupt, "agent_interrupt", map[string]any{
			"task_id": taskID,
			"request": interruptReq,
		}, "interrupt:"+taskID+":"+req.Action)
		ev.Scope = taskID
		if err := s.outboxWriter.Write(r.Context(), ev); err != nil {
			slog.Error("handleAgentInterrupt: outbox write failed, falling back to direct call", "err", err)
		}
	} else if s.agentPool != nil {
		// 单进程降级路径修复（2026-07-12）：此前该分支的注释声称"直接调用"，
		// 实际实现只打一行 INFO 日志——outboxWriter 此前从未在 cmd/polaris 启动
		// 流程中被注入（SetOutboxWriter 从未被调用，见 server_core.go 修复），
		// 意味着本端点在生产环境下恒定静默丢弃中断请求，客户端点击"中止/恢复/
		// 重定向"实际上什么都不会发生。现在真正实现直接调用：与 sse.go
		// handleAgentStreamFSM 客户端断连分支（ctx.Done() → agentCtrl.Interrupt）
		// 使用同一条路径——经 AgentPool.Acquire 取得目标会话的 AgentController
		// 后直接调用 Interrupt，用完立即 release。
		agentCtrl, release, acqErr := s.agentPool.Acquire(r.Context(), taskID)
		if acqErr != nil {
			slog.Error("handleAgentInterrupt: direct-call fallback failed to acquire agent controller", "task_id", taskID, "err", acqErr)
		} else {
			agentCtrl.Interrupt(interruptReq)
			release()
		}
	} else {
		slog.Warn("handleAgentInterrupt: neither outboxWriter nor agentPool available, interrupt request dropped", "task_id", taskID)
	}

	if s.auditTrail != nil {
		detail, _ := json.Marshal(map[string]any{
			"task_id":  taskID,
			"action":   req.Action,
			"redirect": req.Redirect,
			"reason":   req.Reason,
		})
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "accepted",
		"taskID": taskID,
	})
}

// handleResolveApproval 提交审批结果。
func (s *Server) handleResolveApproval(w http.ResponseWriter, r *http.Request) {
	if s.hitlGateway == nil {
		http.Error(w, "HITL not enabled", http.StatusNotImplemented)
		return
	}

	pathParts := strings.Split(r.URL.Path, "/")
	if len(pathParts) < 5 || pathParts[4] != "resolve" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	approvalID := pathParts[3]

	var req struct {
		Action  string `json:"action"` // "approve" or "deny"
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}

	authCtx := authcontext.FromContext(r.Context())

	resp := types.HITLResponse{
		OptionKey: req.Action,
		Approved:  req.Action == "approve",
		Reason:    req.Comment,
		UserID:    authCtx.UserID, // M13: 接入鉴权上下文
	}

	err := s.hitlGateway.Respond(r.Context(), approvalID, resp)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// agentStateString 把 Agent FSM 的 types.AgentState 映射成 WebUI 展示用的小写字符串。
func agentStateString(s types.AgentState) string {
	switch s {
	case types.AgentStateIdle:
		return "idle"
	case types.AgentStatePerceive:
		return "perceive"
	case types.AgentStatePlan:
		return "plan"
	case types.AgentStateValidate:
		return "validate"
	case types.AgentStateExecute:
		return "execute"
	case types.AgentStateReflect:
		return "reflect"
	case types.AgentStateReplan:
		return "replan"
	case types.AgentStateRollback:
		return "rollback"
	case types.AgentStateComplete:
		return "complete"
	case types.AgentStateFailed:
		return "failed"
	case types.AgentStateInterrupt:
		return "interrupt"
	default:
		return "unknown"
	}
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sealed":          sealed,
		"model_id":        modelID,
		"token_used":      0,
		"token_limit":     0,
		"cost_cny":        0.0,
		"memory_mb":       memMB,
		"memory_limit_mb": 8192,
		"agent_id":        agentID,
		"agent_state":     agentState,
		"agent_config":    agentConfig,
	})
}
