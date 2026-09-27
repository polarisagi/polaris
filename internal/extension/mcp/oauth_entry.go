package mcp

// mcpAuthState mcpEntry 的 OAuth 需授权状态快照（不可变，替换整体指针更新）。
// ResourceMetadata/ChallengeScope 缓存最近一次挑战的公开信息，供 BeginAuthorization
// 复用（避免重复走一次无令牌探测请求，见 oauth_flow.go §Begin）。
type mcpAuthState struct {
	Required         bool
	Scopes           []string
	ResourceMetadata string
	ChallengeScope   string
}

// setAuthRequired 置位需授权状态并缓存挑战信息（resourceMetadata 可为空，表示复用旧缓存）。
func (e *mcpEntry) setAuthRequired(scopes []string, resourceMetadata string) {
	prev := e.authState.Load()
	if resourceMetadata == "" && prev != nil {
		resourceMetadata = prev.ResourceMetadata
	}
	e.authState.Store(&mcpAuthState{
		Required:         true,
		Scopes:           append([]string(nil), scopes...),
		ResourceMetadata: resourceMetadata,
		ChallengeScope:   joinScopes(scopes),
	})
}

// clearAuthRequired 授权成功后清除需授权标记，保留挑战缓存供审计/排障。
func (e *mcpEntry) clearAuthRequired() {
	prev := e.authState.Load()
	if prev == nil {
		return
	}
	cp := *prev
	cp.Required = false
	e.authState.Store(&cp)
}

// authInfo 返回 (需授权, 所需 scope 列表)，供 ListServers 填充 MCPServerInfo。
func (e *mcpEntry) authInfo() (bool, []string) {
	if s := e.authState.Load(); s != nil {
		return s.Required, s.Scopes
	}
	return false, nil
}

// cachedChallenge 返回缓存的 (resource_metadata URL, scope 原始字符串)，供
// BeginAuthorization 跳过重复的无令牌探测请求。
func (e *mcpEntry) cachedChallenge() (resourceMetadata, scope string) {
	if s := e.authState.Load(); s != nil {
		return s.ResourceMetadata, s.ChallengeScope
	}
	return "", ""
}

func joinScopes(scopes []string) string {
	out := ""
	for i, s := range scopes {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
