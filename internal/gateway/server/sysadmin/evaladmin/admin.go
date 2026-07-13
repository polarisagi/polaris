// Package evaladmin 承载 V8-S2 Meta-Eval Sentinel（meta_holdout 分区）的运维
// HTTP 接口：写入隔离测试用例、触发签名审计、查询最新审计结论。沿用
// cronadmin/mcpadmin 等已验证过的模式：独立结构体 + 消费方定义的最小接口集 +
// 独立构造函数，父 SysAdminHandler 只持有子结构体指针并做单行转发。
//
// 架构依据：docs/arch/00-Global-Dictionary.md §V8-Principle（S2 外部锚点）、
// internal/eval/control/engine.go（RoleMetaAuditor/PartitionMetaHoldout）、
// internal/eval/analysis/meta_eval.go（MetaEvalSentinel）。
//
// 隔离边界通过签名而非物理进程实现：本包所有写入/审计触发接口都要求请求体
// 携带 meta_auditor 私钥签名的 base64 signature 字段；该私钥只应存在于运维本地
// （见 `polaris eval sign` CLI 子命令），从不出现在运行中 server 的配置/环境
// 变量里——server 侧只持有验签用的公钥（POLARIS_EVAL_PUBKEY_META_AUDITOR）。
// 因此即使这些 handler 运行在自进化引擎所在的同一进程内，服务器自身也无法
// 伪造一次通过的审计：它没有私钥可以产生合法签名。
package evaladmin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/eval/analysis"
	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// EvalStore evaladmin 消费方视角的最小 meta_holdout 数据接口。
// 由 *harness.SQLiteEvalStore 结构性满足，无需适配器。
type EvalStore interface {
	PutMetaHoldoutCase(ctx context.Context, c harness.EvalCase, signature []byte) error
	LatestMetaAudit(ctx context.Context) (passed bool, computedAt time.Time, ok bool, err error)
}

// MetaAuditor evaladmin 消费方视角的最小审计触发接口。
// 由 *analysis.MetaEvalSentinel 结构性满足，无需适配器。
type MetaAuditor interface {
	RunAndRecord(ctx context.Context, signature []byte) (*analysis.MetaEvalResult, error)
}

// EvalAdmin 承载 meta_holdout 用例写入 + 审计触发 + 状态查询。
type EvalAdmin struct {
	Store    EvalStore
	Sentinel MetaAuditor
}

// NewEvalAdmin 构造 EvalAdmin。Store/Sentinel 为 nil 时对应 handler 返回 503，
// 不 panic（与本包其余 handler 及 mcpadmin.HandleCreateMCPServer 的 nil 防御一致）。
func NewEvalAdmin(store EvalStore, sentinel MetaAuditor) *EvalAdmin {
	return &EvalAdmin{Store: store, Sentinel: sentinel}
}

// addMetaHoldoutCaseRequest POST /v1/eval/meta-holdout/cases 请求体。
type addMetaHoldoutCaseRequest struct {
	Case      harness.EvalCase `json:"case"`
	Signature string           `json:"signature"` // base64 Ed25519 签名，由 `polaris eval sign` 离线生成
}

// HandleAddMetaHoldoutCase 写入一条 meta_holdout 隔离测试用例。
// 必须携带有效 meta_auditor 签名，否则 SQLiteEvalStore.PutMetaHoldoutCase 拒绝写入。
func (h *EvalAdmin) HandleAddMetaHoldoutCase(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		http.Error(w, "eval store not initialized", http.StatusServiceUnavailable)
		return
	}
	var req addMetaHoldoutCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request body", err, http.StatusBadRequest)
		return
	}
	sig, err := decodeSignature(req.Signature)
	if err != nil {
		httputil.RespondError(w, "invalid signature encoding", err, http.StatusBadRequest)
		return
	}
	if err := h.Store.PutMetaHoldoutCase(r.Context(), req.Case, sig); err != nil {
		httputil.RespondError(w, "failed to write meta_holdout case", err, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": req.Case.ID}) //nolint:errcheck
}

// runMetaAuditRequest POST /v1/eval/meta-audit 请求体。
type runMetaAuditRequest struct {
	Signature string `json:"signature"`
}

// HandleRunMetaAudit 触发一次 Meta-Eval 审计并持久化结论。
// 必须携带有效 meta_auditor 签名，否则 MetaEvalSentinel.RunMetaEvalSuite 内部的
// GetMetaHoldoutCases 拒绝读取 meta_holdout 数据。这是本包唯一的审计触发入口——
// 不提供"只跑不记录"的裸端点，避免执行与持久化不一致。
func (h *EvalAdmin) HandleRunMetaAudit(w http.ResponseWriter, r *http.Request) {
	if h.Sentinel == nil {
		http.Error(w, "meta-eval sentinel not initialized", http.StatusServiceUnavailable)
		return
	}
	var req runMetaAuditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request body", err, http.StatusBadRequest)
		return
	}
	sig, err := decodeSignature(req.Signature)
	if err != nil {
		httputil.RespondError(w, "invalid signature encoding", err, http.StatusBadRequest)
		return
	}
	result, err := h.Sentinel.RunAndRecord(r.Context(), sig)
	if err != nil {
		httputil.RespondError(w, "meta-audit run failed", err, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result) //nolint:errcheck
}

// HandleGetMetaAuditStatus 返回最新一次持久化的审计结论。
// 只读摘要（pass/fail + 时间戳），不暴露 meta_holdout 原始用例数据，不要求签名——
// 这是给运维仪表盘/CLI status 查询用的低敏感度接口。
func (h *EvalAdmin) HandleGetMetaAuditStatus(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		http.Error(w, "eval store not initialized", http.StatusServiceUnavailable)
		return
	}
	passed, computedAt, ok, err := h.Store.LatestMetaAudit(r.Context())
	if err != nil {
		httputil.RespondError(w, "failed to read meta-audit status", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"recorded":    ok,
		"passed":      passed,
		"computed_at": computedAt,
	})
}

// decodeSignature 将 base64 签名字符串解码为字节。空字符串返回 nil（放行开发模式——
// 未配置公钥时 verifyEvalSignature 仅告警，与既有 Training/Validation 签名校验行为一致）。
func decodeSignature(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, nil
	}
	sig, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "evaladmin: invalid signature base64 encoding", err)
	}
	return sig, nil
}
