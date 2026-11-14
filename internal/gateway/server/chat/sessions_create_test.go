package chat

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type createSessionExpertResolver struct {
	profile *types.AgentProfileSpec
	err     error
}

func (r *createSessionExpertResolver) ResolveNamedExpert(_ context.Context, name string) (*types.AgentProfileSpec, *lifecycle.ExpertDefinition, error) {
	if r.err != nil {
		return nil, nil, r.err
	}
	if r.profile != nil {
		return r.profile, &lifecycle.ExpertDefinition{PluginID: "pl_bundle", PluginName: "bundle", Expert: pluginspec.Expert{ID: "research", Type: "team", EntryAgent: "lead", DisplayName: map[string]string{"en": "Research"}}}, nil
	}
	return &types.AgentProfileSpec{Name: name, Source: "plugin:pl_bundle", Instructions: "role", InstructionTaint: types.TaintMedium},
		&lifecycle.ExpertDefinition{PluginID: "pl_bundle", PluginName: "bundle", Expert: pluginspec.Expert{ID: "research", Type: "team", EntryAgent: "lead", DisplayName: map[string]string{"en": "Research"}}}, nil
}

func TestHandleCreateSessionPersistsExpertBinding(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "expert.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ddl, err := schema.FS.ReadFile("013_chat.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	resolver := &createSessionExpertResolver{}
	h := &ChatHandler{PersistenceService: &ChatPersistenceService{ChatRepo: repo.NewSQLiteChatRepository(db)}, ExpertResolver: resolver}
	r := httptest.NewRequest("POST", "/v1/sessions", bytes.NewBufferString(`{"title":"Research","agent_profile":"bundle:lead"}`))
	w := httptest.NewRecorder()
	h.HandleCreateSession(w, r)
	if w.Code != 201 {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		SessionID    string `json:"session_id"`
		AgentProfile string `json:"agent_profile"`
		ExpertID     string `json:"expert_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.SessionID == "" || body.AgentProfile != "bundle:lead" || body.ExpertID != "research" {
		t.Fatalf("unexpected response: %#v", body)
	}
	row, err := h.PersistenceService.ChatRepo.GetSession(context.Background(), body.SessionID)
	if err != nil || row == nil || row.AgentProfileSource != "plugin:pl_bundle" || row.AgentProfileDigest == "" || row.ExpertID != "research" {
		t.Fatalf("binding not persisted: row=%#v err=%v", row, err)
	}
	if _, err := h.ResolveBoundProfile(context.Background(), body.SessionID); err != nil {
		t.Fatalf("first turn binding rejected: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// 新连接/新 Handler 模拟进程重启：Profile 必须从落盘绑定重新解析。
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restarted := &ChatHandler{PersistenceService: &ChatPersistenceService{ChatRepo: repo.NewSQLiteChatRepository(db)}, ExpertResolver: resolver}
	if _, err := restarted.ResolveBoundProfile(context.Background(), body.SessionID); err != nil {
		t.Fatalf("restart lost bound profile: %v", err)
	}
	resolver.profile = &types.AgentProfileSpec{Name: "bundle:lead", Source: "plugin:pl_bundle", Instructions: "changed content", InstructionTaint: types.TaintMedium}
	if _, err := restarted.ResolveBoundProfile(context.Background(), body.SessionID); !apperr.IsCode(err, apperr.CodeConflict) {
		t.Fatalf("changed profile must fail closed, got %v", err)
	}
	resolver.profile = nil
	resolver.err = apperr.New(apperr.CodeNotFound, "plugin disabled or removed")
	if _, err := restarted.ResolveBoundProfile(context.Background(), body.SessionID); !apperr.IsCode(err, apperr.CodeConflict) {
		t.Fatalf("unavailable binding must require new session, got %v", err)
	}
	resolver.err = apperr.New(apperr.CodeInternal, "plugin repository unavailable")
	if _, err := restarted.ResolveBoundProfile(context.Background(), body.SessionID); !apperr.IsCode(err, apperr.CodeInternal) {
		t.Fatalf("resolver outage must preserve storage error, got %v", err)
	}
}
