package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// failingUpsertSystemRepo 的 UpsertPreference 总是失败，其余方法返回零值，
// 用于验证阶段02修复（§2.8）：system_prompt_template 落库失败时必须
// Error 级可见 + 置位 systemPromptDegraded，供 /healthz 暴露，而不是像
// 修复前 `_ = s.systemRepo.UpsertPreference(...)` 那样彻底静默。
type failingUpsertSystemRepo struct{}

func (r *failingUpsertSystemRepo) GetPreference(ctx context.Context, key string) (string, error) {
	return "", apperr.New(apperr.CodeNotFound, "not found")
}
func (r *failingUpsertSystemRepo) ListPreferences(ctx context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *failingUpsertSystemRepo) UpsertPreference(ctx context.Context, key, value string) error {
	return apperr.New(apperr.CodeInternal, "simulated preference write failure")
}
func (r *failingUpsertSystemRepo) DeletePreference(ctx context.Context, key string) error {
	return nil
}
func (r *failingUpsertSystemRepo) UpsertKV(ctx context.Context, key, value string) error {
	return nil
}
func (r *failingUpsertSystemRepo) RestoreKV(ctx context.Context, key, value, updatedAt string) error {
	return nil
}
func (r *failingUpsertSystemRepo) UpsertVFSRef(ctx context.Context, vfsURI string, blobSize int64, createdAt int64) error {
	return nil
}

// stubPromptFacade 提供 SetPromptManager 所需的最小实现。
type stubPromptFacade struct{}

func (p *stubPromptFacade) ReadPrompt(name, fallback string) string             { return fallback }
func (p *stubPromptFacade) ReadPromptDefault(name string) string                { return "系统提示词默认值" }
func (p *stubPromptFacade) ModelSpecificGuidance(modelID string) string         { return "" }
func (p *stubPromptFacade) WriteUserPrompt(name, content string) error          { return nil }
func (p *stubPromptFacade) DeleteUserPrompt(name string) error                  { return nil }
func (p *stubPromptFacade) DefaultIdentity() string                             { return "" }
func (p *stubPromptFacade) GetSoulMD() string                                   { return "" }
func (p *stubPromptFacade) PlatformHintFor(platform string) string              { return "" }
func (p *stubPromptFacade) Optimize(ctx context.Context, taskType string) error { return nil }

var _ protocol.PromptFacade = (*stubPromptFacade)(nil)

func TestSetPromptManager_UpsertPreferenceFailure_SetsDegradedFlag_S02(t *testing.T) {
	s := &Server{systemRepo: &failingUpsertSystemRepo{}}

	s.SetPromptManager(&stubPromptFacade{})

	if !s.systemPromptDegraded.Load() {
		t.Error("expected systemPromptDegraded to be set after UpsertPreference failure")
	}
	// 内存中当前请求仍应正常使用新模板（不因落库失败而回退到残缺兜底版）。
	if s.baseSystemPromptTpl != "系统提示词默认值" {
		t.Errorf("expected baseSystemPromptTpl to still be set to the newly read default, got %q", s.baseSystemPromptTpl)
	}
}

func TestHandleHealthz_ExposesDegradedSystemPrompt_S02(t *testing.T) {
	s := &Server{}
	s.systemPromptDegraded.Store(true)

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	s.handleHealthz(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `"degraded_system_prompt":true`) {
		t.Errorf("expected body to report degraded_system_prompt=true, got %s", body)
	}
}
