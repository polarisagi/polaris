package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/pkg/types"
)

// handleHealthz 提供基础的健康检查。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleReadyz Kubernetes readiness probe 端点。
// 就绪（isReady=true）返回 200，未就绪返回 503。
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !s.isReady.Load() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"not_ready"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready"}`))
} //nolint:errcheck

// handleGetConfig 返回当前运行时配置的原始内容（只读视图）。
//
// 读取优先级：
//  1. POLARIS_CONFIG 环境变量指向的文件（Operator 运行时覆盖）
//  2. 二进制内嵌的 configs/defaults.toml（embedded FS，始终可用）
//
// 使用 embedded FS 而非相对路径 os.ReadFile，确保二进制在任意工作目录下均可运行。
//
//nolint:unused
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	var (
		raw    []byte
		source string
		err    error
	)

	if cfgPath := os.Getenv("POLARIS_CONFIG"); cfgPath != "" {
		// Operator 显式指定配置文件：从文件系统读取，路径可为绝对路径或相对路径。
		raw, err = os.ReadFile(cfgPath)
		if err != nil {
			http.Error(w, "POLARIS_CONFIG file not readable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		source = cfgPath
	} else {
		// 默认：从 binary 内嵌 FS 读取（configs/embed.go //go:embed *.toml ...）。
		// 此路径不依赖工作目录，任意部署环境均可用。
		raw, err = configs.FS.ReadFile("defaults.toml")
		if err != nil {
			// embedded FS 读取失败属于编译期资产问题，用 500 而非 404。
			http.Error(w, "embedded config not readable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		source = "embedded:configs/defaults.toml"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path":   source,
		"format": "toml",
		"raw":    string(raw),
	})
}

// handleEvalRun 触发 M12 评测套件执行并返回报告。
// POST /v1/eval/run  body: {"suite":"training"|"validation"}
// 2026-07-14（ADR-0051 关联接线）：路由注册见 server_routes.go。
func (s *Server) handleEvalRun(w http.ResponseWriter, r *http.Request) {
	if s.evalRunner == nil {
		http.Error(w, "eval runner not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Suite       string `json:"suite"`
		CandidateID string `json:"candidate_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	if req.Suite == "" {
		req.Suite = "training"
	}
	report, err := s.evalRunner.RunSuite(r.Context(), req.Suite, req.CandidateID)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}

// handleAgentQuery 将用户查询发布为异步 Blackboard Task，立即返回 task_id。
// 调用方通过 GET /v1/agent/tasks/{taskID} 轮询结果（HE-Rule-5 FSM 控制流）。
func (s *Server) handleAgentQuery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input     string `json:"input"`
		SessionID string `json:"session_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		http.Error(w, "input must not be empty", http.StatusBadRequest)
		return
	}

	if s.blackboard == nil {
		// Blackboard 未注入时退化：直接注入 Agent Intent，返回兼容响应
		if s.agentPool != nil {
			agent, release, err := s.agentPool.Acquire(r.Context(), "default")
			if err == nil {
				agent.SetTaskIntent([]byte(req.Input))
				release()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task_id": "",
			"status":  "pending",
			"note":    "blackboard not available; intent injected directly",
		})
		return
	}

	now := time.Now().UnixMilli()
	task := &types.TaskEntry{
		ID:          "task-" + uuid.NewString(),
		Type:        "agent_query",
		Priority:    0,
		Status:      types.TaskPending,
		Intent:      []byte(req.Input),
		IntentTaint: types.TaintMedium, // 外部用户输入，中等置信度
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.blackboard.PostTask(r.Context(), task); err != nil {
		slog.Error("handleAgentQuery: PostTask failed", "error", err)
		http.Error(w, "failed to submit task", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted) // 202 Accepted
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task_id": task.ID,
		"status":  "pending",
	})
}

// handleGetAgentTask 查询 Blackboard 中指定 task 的当前状态快照。
// GET /v1/agent/tasks/{taskID}
func (s *Server) handleGetAgentTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("taskID")
	if taskID == "" {
		http.Error(w, "taskID is required", http.StatusBadRequest)
		return
	}

	if s.blackboard == nil {
		http.Error(w, "blackboard not available", http.StatusNotImplemented)
		return
	}

	snap, err := s.blackboard.PeekTask(r.Context(), taskID)
	if err != nil {
		slog.Warn("handleGetAgentTask: PeekTask failed", "task_id", taskID, "error", err)
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}

// handleGetPendingApprovals/parseInterruptAction/handleAgentInterrupt/
// handleResolveApproval（HITL 审批 + 中断处理）、agentStateString/
// handleStatus（WebUI 状态快照）见 server_handlers_hitl.go（R7 拆分）。
