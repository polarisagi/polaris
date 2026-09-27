package mcp

import (
	"encoding/json"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// rowOAuthConfig mcp_servers.oauth 列的存储形态（预注册 OAuth 客户端配置）：
// {"client_id","client_secret_enc","auth_server_metadata_url","scopes":[...]}。
// client_secret 经 credential.Vault 加密后存 client_secret_enc，不落明文（015_mcp_servers.sql）。
type rowOAuthConfig struct {
	ClientID              string   `json:"client_id,omitempty"`
	ClientSecretEnc       string   `json:"client_secret_enc,omitempty"`
	AuthServerMetadataURL string   `json:"auth_server_metadata_url,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
}

// parseRowOAuthConfig 解析 mcp_servers.oauth 列；空/"{}" 返回 (nil, nil)。
func parseRowOAuthConfig(raw string) (*rowOAuthConfig, error) {
	if raw == "" || raw == "{}" {
		return nil, nil
	}
	var cfg rowOAuthConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "mcp_manager: mcp_servers.oauth column", err)
	}
	if cfg.ClientID == "" && cfg.AuthServerMetadataURL == "" && len(cfg.Scopes) == 0 {
		return nil, nil
	}
	return &cfg, nil
}

// RowOAuthConfig 导出别名，供网关管理层（internal/gateway/server/sysadmin/mcpadmin）读写
// mcp_servers.oauth 列——网关侧渲染 GET /v1/mcp-servers 的 oauth 摘要字段、处理
// PUT .../oauth 预注册配置，均需复用同一 JSON 形状，不在网关侧另写一份（HE-3 单一定义）。
type RowOAuthConfig = rowOAuthConfig

// ParseRowOAuthConfig 导出包装，网关侧用于读取 mcp_servers.oauth 列（列出预注册摘要、
// 更新前保留未修改字段）。raw 为空/"{}" 返回 (nil, nil)。
func ParseRowOAuthConfig(raw string) (*RowOAuthConfig, error) {
	return parseRowOAuthConfig(raw)
}

// MarshalRowOAuthConfig 序列化预注册配置为 mcp_servers.oauth 列 JSON；cfg 为 nil 时
// 返回 "{}"（PUT .../oauth 显式清空预注册配置的场景）。
func MarshalRowOAuthConfig(cfg *RowOAuthConfig) string {
	if cfg == nil {
		return "{}"
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// BuildRowOAuthJSON 把 pluginspec 归一化的预注册 OAuth 声明序列化为 mcp_servers.oauth 列 JSON。
// 插件清单只声明公开信息（client_id/auth_server_metadata_url/scopes）；client_secret_enc
// 留空——机密客户端的密钥需管理员经独立配置接口写入，不会出现在插件安装路径的明文清单里。
// 供 internal/extension/lifecycle 的 mcpRowFromSpec 在安装/更新行时调用（与 headers 同一位置）。
func BuildRowOAuthJSON(o *pluginspec.MCPOAuth) string {
	if o == nil {
		return "{}"
	}
	cfg := rowOAuthConfig{ClientID: o.ClientID, AuthServerMetadataURL: o.AuthServerMetadataURL, Scopes: o.Scopes}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "{}"
	}
	return string(b)
}
