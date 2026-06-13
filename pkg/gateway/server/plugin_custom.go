package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/extensions/marketplace"
)

// handleCreateSkill 用户手动创建 Skill 扩展。
// POST /v1/skills/create
func (s *Server) handleCreateSkill(w http.ResponseWriter, r *http.Request) { //nolint:nestif
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		RepoURL     string `json:"repo_url"`
		Entrypoint  string `json:"entrypoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	extID := "ext_" + newHex(8)

	if s.installMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtx0 := FromContext(r.Context())
	principal0 := authCtx0.UserID
	if principal0 == "" {
		principal0 = "user"
	}
	installReq0 := marketplace.InstallRequest{
		Principal:   principal0,
		ExtensionID: extID,
		ExtType:     "skill",
		TrustTier:   1, // TrustLocal
		Publisher:   "user",
		HasHooks:    false,
	}
	if err := s.installMgr.Authorize(r.Context(), installReq0); err != nil { //nolint:nestif
		if errors.Is(err, marketplace.ErrRequiresApproval) {
			if s.hitlGateway != nil {
				_, _ = s.hitlGateway.Prompt(r.Context(), protocol.HITLPrompt{
					ID:             extID,
					CheckpointType: "security_review",
					PromptText:     "Approve creation for custom skill: " + req.Name,
					Options: []protocol.HITLOption{
						{Key: "approve", Label: "Approve"},
						{Key: "deny", Label: "Deny"},
					},
				})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	configJSON, _ := json.Marshal(map[string]any{
		"repo_url":   req.RepoURL,
		"entrypoint": req.Entrypoint,
	})

	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO extension_instances
		 (id, ext_type, origin, catalog_id, name, publisher, trust_tier,
		  runtime_id, install_path, config, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,'','',?,'installed',?,?)`,
		extID, "skill", "user", "",
		req.Name, "user", 1,
		string(configJSON), now, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "name": req.Name, "type": "skill",
	})
}

// handleCreatePlugin 用户手动创建 Plugin 扩展（manifest_url 模式）。
// POST /v1/plugins/create
func (s *Server) handleCreatePlugin(w http.ResponseWriter, r *http.Request) { //nolint:nestif
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ManifestURL string `json:"manifest_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	extID := "ext_" + newHex(8)

	if s.installMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtx1 := FromContext(r.Context())
	principal1 := authCtx1.UserID
	if principal1 == "" {
		principal1 = "user"
	}
	installReq1 := marketplace.InstallRequest{
		Principal:   principal1,
		ExtensionID: extID,
		ExtType:     "plugin",
		TrustTier:   1, // TrustLocal
		Publisher:   "user",
		HasHooks:    false,
	}
	if err := s.installMgr.Authorize(r.Context(), installReq1); err != nil { //nolint:nestif
		if errors.Is(err, marketplace.ErrRequiresApproval) {
			if s.hitlGateway != nil {
				_, _ = s.hitlGateway.Prompt(r.Context(), protocol.HITLPrompt{
					ID:             extID,
					CheckpointType: "security_review",
					PromptText:     "Approve creation for custom plugin: " + req.Name,
					Options: []protocol.HITLOption{
						{Key: "approve", Label: "Approve"},
						{Key: "deny", Label: "Deny"},
					},
				})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	configJSON, _ := json.Marshal(map[string]any{
		"manifest_url": req.ManifestURL,
	})

	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO extension_instances
		 (id, ext_type, origin, catalog_id, name, publisher, trust_tier,
		  runtime_id, install_path, config, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,'','',?,'installed',?,?)`,
		extID, "plugin", "user", "",
		req.Name, "user", 1,
		string(configJSON), now, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "name": req.Name, "type": "plugin",
	})
}

// handleCreateApp 用户手动创建 App 扩展（URL 模式）。
// POST /v1/apps/create
func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) { //nolint:nestif
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		URL         string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	extID := "ext_" + newHex(8)

	if s.installMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtx2 := FromContext(r.Context())
	principal2 := authCtx2.UserID
	if principal2 == "" {
		principal2 = "user"
	}
	installReq2 := marketplace.InstallRequest{
		Principal:   principal2,
		ExtensionID: extID,
		ExtType:     "app",
		TrustTier:   1, // TrustLocal
		Publisher:   "user",
		HasHooks:    false,
	}
	if err := s.installMgr.Authorize(r.Context(), installReq2); err != nil { //nolint:nestif
		if errors.Is(err, marketplace.ErrRequiresApproval) {
			if s.hitlGateway != nil {
				_, _ = s.hitlGateway.Prompt(r.Context(), protocol.HITLPrompt{
					ID:             extID,
					CheckpointType: "security_review",
					PromptText:     "Approve creation for custom app: " + req.Name,
					Options: []protocol.HITLOption{
						{Key: "approve", Label: "Approve"},
						{Key: "deny", Label: "Deny"},
					},
				})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	// 写 apps 运行时表（与 MCP→mcp_servers / Skill→skills 的模式一致）
	appID := "app_" + newHex(8)
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO apps(id, name, display_name, description, url, publisher, enabled, trust_tier, catalog_id, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,1,1,'',?,?)`,
		appID, req.Name, req.Name, req.Description, req.URL, "user", now, now)
	if err != nil {
		http.Error(w, "apps insert: "+err.Error(), http.StatusInternalServerError)
		return
	}

	configJSON, _ := json.Marshal(map[string]any{"url": req.URL})
	_, err = s.db.ExecContext(r.Context(),
		`INSERT INTO extension_instances
		 (id, ext_type, origin, catalog_id, name, publisher, trust_tier,
		  runtime_id, install_path, config, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,'',?,'installed',?,?)`,
		extID, "app", "user", "",
		req.Name, "user", 1, appID,
		string(configJSON), now, now)
	if err != nil {
		// 回滚 apps 插入
		_, _ = s.db.ExecContext(r.Context(), "DELETE FROM apps WHERE id=?", appID)
		http.Error(w, "extension_instances insert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "app_id": appID, "name": req.Name, "type": "app",
	})
}

// handleCreateMCP 用户手动配置 MCP Server。
// POST /v1/mcp/create
// MCP 需要实时连接，同时写 mcp_servers（运行时）和 extension_instances（安装 SSoT）。
func (s *Server) handleCreateMCP(w http.ResponseWriter, r *http.Request) { //nolint:nestif
	var req struct {
		Name      string            `json:"name"`
		Transport string            `json:"transport"`
		Command   string            `json:"command"`
		Args      []string          `json:"args"`
		Env       map[string]string `json:"env"`
		URL       string            `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	mcpID := "mcp_" + newHex(8)
	extID := "ext_" + newHex(8)

	if s.installMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtx3 := FromContext(r.Context())
	principal3 := authCtx3.UserID
	if principal3 == "" {
		principal3 = "user"
	}
	installReq3 := marketplace.InstallRequest{
		Principal:   principal3,
		ExtensionID: extID,
		ExtType:     "mcp",
		TrustTier:   1, // TrustLocal
		Publisher:   "user",
		HasHooks:    false,
	}
	if err := s.installMgr.Authorize(r.Context(), installReq3); err != nil { //nolint:nestif
		if errors.Is(err, marketplace.ErrRequiresApproval) {
			if s.hitlGateway != nil {
				_, _ = s.hitlGateway.Prompt(r.Context(), protocol.HITLPrompt{
					ID:             extID,
					CheckpointType: "security_review",
					PromptText:     "Approve creation for custom mcp: " + req.Name,
					Options: []protocol.HITLOption{
						{Key: "approve", Label: "Approve"},
						{Key: "deny", Label: "Deny"},
					},
				})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	argsBytes, _ := json.Marshal(req.Args)
	envBytes, _ := json.Marshal(req.Env)

	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO mcp_servers(id, name, transport, command, args, env, url, enabled, timeout, trust_tier, catalog_id, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,1,30,1,'',?,?)`,
		mcpID, req.Name, req.Transport, req.Command,
		string(argsBytes), string(envBytes), req.URL, now, now)
	if err != nil {
		http.Error(w, "mcp_servers insert: "+err.Error(), http.StatusInternalServerError)
		return
	}

	configJSON, _ := json.Marshal(map[string]any{
		"transport": req.Transport,
		"command":   req.Command,
		"args":      req.Args,
		"env":       req.Env,
		"url":       req.URL,
	})

	_, err = s.db.ExecContext(r.Context(),
		`INSERT INTO extension_instances
		 (id, ext_type, origin, catalog_id, name, publisher, trust_tier,
		  runtime_id, install_path, config, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,1,?,?,'{}','installed',?,?)`,
		extID, "mcp", "user", "",
		req.Name, "user",
		mcpID, string(configJSON), now, now)
	if err != nil {
		// 回滚 mcp_servers
		s.db.ExecContext(r.Context(), `DELETE FROM mcp_servers WHERE id=?`, mcpID) //nolint:errcheck
		http.Error(w, "extension_instances insert: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if s.mcpMgr != nil {
		go s.startMCPServer(MCPServerConfig{
			ID:        mcpID,
			Name:      req.Name,
			Transport: req.Transport,
			Command:   req.Command,
			Args:      req.Args,
			Env:       req.Env,
			URL:       req.URL,
			Timeout:   30,
			TrustTier: 1,
			Enabled:   true,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "mcp_id": mcpID, "name": req.Name, "type": "mcp",
	})
}

// newHex 生成 n 字节的随机十六进制字符串。
func newHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
