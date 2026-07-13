package evaladmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/eval/analysis"
	"github.com/polarisagi/polaris/internal/eval/harness"
)

// fakeEvalStore/fakeMetaAuditor 是本包消费方接口（EvalStore/MetaAuditor）的
// 最小内存实现，用于在不依赖真实 SQLiteEvalStore/MetaEvalSentinel 的情况下
// 单独验证 HTTP handler 的请求解析、错误映射与 nil 防御行为。
type fakeEvalStore struct {
	putErr           error
	lastPutCase      harness.EvalCase
	lastPutSignature []byte

	latestPassed     bool
	latestComputedAt time.Time
	latestOK         bool
	latestErr        error
}

func (f *fakeEvalStore) PutMetaHoldoutCase(_ context.Context, c harness.EvalCase, signature []byte) error {
	f.lastPutCase = c
	f.lastPutSignature = signature
	return f.putErr
}

func (f *fakeEvalStore) LatestMetaAudit(context.Context) (bool, time.Time, bool, error) {
	return f.latestPassed, f.latestComputedAt, f.latestOK, f.latestErr
}

type fakeMetaAuditor struct {
	result *analysis.MetaEvalResult
	err    error
}

func (f *fakeMetaAuditor) RunAndRecord(context.Context, []byte) (*analysis.MetaEvalResult, error) {
	return f.result, f.err
}

// ── nil 依赖防御：与 mcpadmin/cronadmin 等既有子包一致，未接线时返回 503 而非 panic ──

func TestHandleAddMetaHoldoutCase_NilStore_Returns503(t *testing.T) {
	h := NewEvalAdmin(nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/meta-holdout/cases", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.HandleAddMetaHoldoutCase(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestHandleRunMetaAudit_NilSentinel_Returns503(t *testing.T) {
	h := NewEvalAdmin(nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/meta-audit", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.HandleRunMetaAudit(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestHandleGetMetaAuditStatus_NilStore_Returns503(t *testing.T) {
	h := NewEvalAdmin(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/eval/meta-audit", nil)
	w := httptest.NewRecorder()
	h.HandleGetMetaAuditStatus(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── 正常路径：请求体解析 + 转发给消费方接口 ────────────────────────────────────

func TestHandleAddMetaHoldoutCase_ForwardsToStore(t *testing.T) {
	store := &fakeEvalStore{}
	h := NewEvalAdmin(store, nil)
	body := `{"case":{"id":"case-1","falsifiability_score":0.8},"signature":""}`
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/meta-holdout/cases", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleAddMetaHoldoutCase(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.lastPutCase.ID != "case-1" {
		t.Fatalf("expected case forwarded to store, got %+v", store.lastPutCase)
	}
}

func TestHandleAddMetaHoldoutCase_StoreErrorMapsTo403(t *testing.T) {
	store := &fakeEvalStore{putErr: errForbidden("bad signature")}
	h := NewEvalAdmin(store, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/meta-holdout/cases", strings.NewReader(`{"case":{"id":"x"}}`))
	w := httptest.NewRecorder()
	h.HandleAddMetaHoldoutCase(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestHandleAddMetaHoldoutCase_InvalidSignatureEncoding_Returns400(t *testing.T) {
	store := &fakeEvalStore{}
	h := NewEvalAdmin(store, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/meta-holdout/cases", strings.NewReader(`{"case":{"id":"x"},"signature":"not-valid-base64!!"}`))
	w := httptest.NewRecorder()
	h.HandleAddMetaHoldoutCase(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleRunMetaAudit_ForwardsResultAsJSON(t *testing.T) {
	sentinel := &fakeMetaAuditor{result: &analysis.MetaEvalResult{Passed: true, TotalCases: 5}}
	h := NewEvalAdmin(nil, sentinel)
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/meta-audit", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.HandleRunMetaAudit(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got analysis.MetaEvalResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !got.Passed || got.TotalCases != 5 {
		t.Fatalf("unexpected response body: %+v", got)
	}
}

func TestHandleGetMetaAuditStatus_ReturnsRecordedFalseWhenNeverAudited(t *testing.T) {
	store := &fakeEvalStore{latestOK: false}
	h := NewEvalAdmin(store, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/eval/meta-audit", nil)
	w := httptest.NewRecorder()
	h.HandleGetMetaAuditStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if recorded, _ := got["recorded"].(bool); recorded {
		t.Fatal("expected recorded=false when never audited")
	}
}

// ── decodeSignature ────────────────────────────────────────────────────────────

func TestDecodeSignature_EmptyStringReturnsNil(t *testing.T) {
	sig, err := decodeSignature("")
	if err != nil || sig != nil {
		t.Fatalf("expected (nil, nil) for empty string, got (%v, %v)", sig, err)
	}
}

func TestDecodeSignature_InvalidBase64ReturnsError(t *testing.T) {
	if _, err := decodeSignature("not-valid-base64!!"); err == nil {
		t.Fatal("expected error for invalid base64 signature")
	}
}

// errForbidden 是一个满足 error 接口的最小占位类型，仅用于本文件测试
// HandleAddMetaHoldoutCase 对 Store 错误的状态码映射（handler 侧统一走
// httputil.RespondError，固定映射为 403，不依赖具体的 apperr.Code）。
type errForbidden string

func (e errForbidden) Error() string { return string(e) }
