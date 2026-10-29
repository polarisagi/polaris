package plugin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type oneSkillRegistry struct{ meta types.SkillMeta }

func (r oneSkillRegistry) Register(context.Context, types.SkillMeta) error { return nil }
func (r oneSkillRegistry) Get(_ context.Context, name, _ string) (*types.SkillMeta, error) {
	if name == r.meta.Name {
		m := r.meta
		return &m, nil
	}
	return nil, nil
}
func (r oneSkillRegistry) List(context.Context, types.SkillFilter) ([]types.SkillMeta, error) {
	return []types.SkillMeta{r.meta}, nil
}
func (r oneSkillRegistry) Deprecate(context.Context, string, string, string) error { return nil }

func TestSkillInjectionReviewFlow(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	db := h.DB.(*sql.DB)
	if _, err := db.Exec(`CREATE TABLE hook_trust (source_key TEXT PRIMARY KEY, digest TEXT NOT NULL, trusted_at TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	h.ExtRepo = repo.NewSQLiteExtensionRepository(db)
	h.SkillReg = oneSkillRegistry{meta: types.SkillMeta{Name: "skill:pr", Instructions: "Diff: !`git diff`"}}
	h.HookRunner = nil
	digest := pluginspec.InjectionDigest(pluginspec.FindInjections("Diff: !`git diff`"))

	list := func() []skillInjectionView {
		w := httptest.NewRecorder()
		h.HandleListSkillInjections(w, httptest.NewRequest("GET", "/v1/skills/injections", nil))
		var out struct {
			Skills []skillInjectionView `json:"skills"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		return out.Skills
	}
	if got := list(); len(got) != 1 || got[0].Trusted || got[0].Commands[0] != "git diff" || got[0].Digest != digest {
		t.Fatalf("pending skill: %+v", got)
	}
	trust := func(d string) int {
		body, _ := json.Marshal(map[string]string{"skill": "skill:pr", "digest": d})
		w := httptest.NewRecorder()
		h.HandleTrustSkillInjection(w, httptest.NewRequest("POST", "/v1/skills/injections/trust", bytes.NewReader(body)))
		return w.Code
	}
	// HookRunner 为空时管理接口不可用（与 hooks 审阅共用授权入口）。
	if code := trust(digest); code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without hook engine, got %d", code)
	}
	h2, _ := newHookHandler(t)
	h.HookRunner = h2.HookRunner
	if code := trust("stale"); code != http.StatusConflict {
		t.Fatalf("stale digest: %d", code)
	}
	if code := trust(digest); code != http.StatusOK || !list()[0].Trusted {
		t.Fatalf("trust: %d", code)
	}
}
