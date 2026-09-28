package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// maxUIResourceBytes MCP Apps UI 资源 HTML 正文大小上限（apps_spec.mdx 未规定具体
// 数值，5 MiB 是本任务说明给出的门控，防止单个视图把网关内存/传输拖垮）。
const maxUIResourceBytes = 5 * 1024 * 1024

// httpsOriginPattern CSP 域名白名单校验：仅接受 https 源，允许 "*." 前缀通配子域
// （apps_spec.mdx §UIResourceMeta 示例："https://*.example.com"）。
var httpsOriginPattern = regexp.MustCompile(`^https://(\*\.)?[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?(:[0-9]{1,5})?$`)

// uiResourceCacheKey ReadUIResource 缓存键：(serverID, uri) 唯一确定一份 UI 资源内容。
type uiResourceCacheKey struct {
	serverID string
	uri      string
}

// clearUIResourceCache 清空指定 server 的全部缓存条目（工具列表刷新或重连时调用，
// 见 doRefreshTools / Add）。资源内容可能随服务器重启/工具刷新而变化，不清空会让
// 前端宿主长期渲染陈旧模板。
func (m *MCPManager) clearUIResourceCache(serverID string) {
	m.uiResourceCache.Range(func(k, _ any) bool {
		if key, ok := k.(uiResourceCacheKey); ok && key.serverID == serverID {
			m.uiResourceCache.Delete(k)
		}
		return true
	})
}

// ReadUIResource 读取一个 MCP Apps UI 资源（apps_spec.mdx §UI Resource Format），
// 经 ResourcesRead 走既有 MRTR 请求路径（Taint 解码保持——复用同一资源读取
// 通道而非另开一条不受既有污点/重试语义约束的旁路）。按 (serverID, uri) 缓存。
func (m *MCPManager) ReadUIResource(ctx context.Context, serverID, uri string) (protocol.UIResource, error) {
	if !strings.HasPrefix(uri, uiResourceURIScheme) {
		return protocol.UIResource{}, apperr.New(apperr.CodeInvalidInput, "mcp apps: uri must start with ui://: "+uri)
	}
	key := uiResourceCacheKey{serverID: serverID, uri: uri}
	if v, ok := m.uiResourceCache.Load(key); ok {
		res, _ := v.(protocol.UIResource)
		return res, nil
	}

	m.mu.RLock()
	e, ok := m.entries[serverID]
	m.mu.RUnlock()
	if !ok || e.client == nil {
		return protocol.UIResource{}, apperr.New(apperr.CodeNotFound, "mcp apps: server not connected: "+serverID)
	}

	start := time.Now()
	contents, err := e.client.ResourcesRead(ctx, uri)
	if err != nil {
		return protocol.UIResource{}, apperr.Wrap(apperr.CodeInternal, "mcp apps: resources/read "+uri, err)
	}
	content, err := pickUIResourceContent(contents, uri)
	if err != nil {
		return protocol.UIResource{}, err
	}
	html, err := decodeUIResourceHTML(content)
	if err != nil {
		return protocol.UIResource{}, err
	}

	resource := protocol.UIResource{HTML: html, MimeType: content.MIMEType}
	parseUIResourceMeta(&resource, content.Meta, serverID, uri)

	slog.Info("mcp apps: ui resource read", "server", serverID, "uri", uri,
		"bytes", len(html), "latency_ms", time.Since(start).Milliseconds())
	m.uiResourceCache.Store(key, resource)
	return resource, nil
}

// pickUIResourceContent 从资源读取返回的内容块列表中选出目标 UI 资源，
// 并校验 mimeType 必须是标准值或 ChatGPT 旧别名（apps_spec.mdx §Content
// Requirements）。服务器未回显 uri 字段但只返回一块内容时按该块处理（部分
// 实现的已知偏差，不因此拒绝整个资源）。
func pickUIResourceContent(contents []protocol.MCPResourceContent, uri string) (protocol.MCPResourceContent, error) {
	var matched *protocol.MCPResourceContent
	for i := range contents {
		if contents[i].URI == uri {
			matched = &contents[i]
			break
		}
	}
	if matched == nil && len(contents) == 1 {
		matched = &contents[0]
	}
	if matched == nil {
		return protocol.MCPResourceContent{}, apperr.New(apperr.CodeNotFound, "mcp apps: ui resource content not found for uri "+uri)
	}
	if matched.MIMEType != mimeTypeMCPApp && matched.MIMEType != mimeTypeSkybridge {
		return protocol.MCPResourceContent{}, apperr.New(apperr.CodeInvalidInput, "mcp apps: unsupported ui resource mime type: "+matched.MIMEType)
	}
	return *matched, nil
}

// decodeUIResourceHTML text/blob 二选一解码（apps_spec.mdx §Content Requirements），
// 并强制 5 MiB 上限。
func decodeUIResourceHTML(content protocol.MCPResourceContent) (string, error) {
	var raw []byte
	switch {
	case content.Text != "":
		raw = []byte(content.Text)
	case content.Blob != "":
		decoded, err := decodeBase64(content.Blob)
		if err != nil {
			return "", apperr.Wrap(apperr.CodeInvalidInput, "mcp apps: ui resource blob decode", err)
		}
		raw = decoded
	default:
		return "", apperr.New(apperr.CodeInvalidInput, "mcp apps: ui resource has neither text nor blob")
	}
	if len(raw) > maxUIResourceBytes {
		return "", apperr.New(apperr.CodeInvalidInput,
			fmt.Sprintf("mcp apps: ui resource html too large: %d bytes exceeds %d limit", len(raw), maxUIResourceBytes))
	}
	return string(raw), nil
}

// uiResourceMetaContent 内容级 _meta.ui 结构（apps_spec.mdx §UIResourceMeta）。
type uiResourceMetaContent struct {
	UI struct {
		CSP struct {
			ConnectDomains  []string `json:"connectDomains"`
			ResourceDomains []string `json:"resourceDomains"`
			FrameDomains    []string `json:"frameDomains"`
			BaseURIDomains  []string `json:"baseUriDomains"`
		} `json:"csp"`
		// Permissions 键存在即视为请求该权限（值本身是空对象 {}，presence-based，
		// 见 apps_spec.mdx §UIResourceMeta permissions 注释）。
		Permissions   map[string]json.RawMessage `json:"permissions"`
		Domain        string                     `json:"domain"`
		PrefersBorder *bool                      `json:"prefersBorder"`
	} `json:"ui"`
}

// openaiWidgetCSPMeta ChatGPT 别名 _meta["openai/widgetCSP"]（openai_mcp_apps.md）。
type openaiWidgetCSPMeta struct {
	WidgetCSP struct {
		ConnectDomains  []string `json:"connect_domains"`
		ResourceDomains []string `json:"resource_domains"`
		FrameDomains    []string `json:"frame_domains"`
	} `json:"openai/widgetCSP"`
}

// parseUIResourceMeta 解析内容级 _meta.ui：标准字段优先，仅当标准 csp 三个核心域名
// 字段（connect/resource/frame）全部缺省时才采用 openai/widgetCSP 别名（与工具级
// ParseToolUI 同一"标准优先"原则）。非法 JSON 静默跳过（记 Warn），不阻断资源本身。
func parseUIResourceMeta(res *protocol.UIResource, meta json.RawMessage, serverID, uri string) {
	if len(meta) == 0 {
		return
	}
	var m uiResourceMetaContent
	if err := json.Unmarshal(meta, &m); err != nil {
		slog.Warn("mcp apps: ui resource _meta parse failed", "server", serverID, "uri", uri, "err", err)
		return
	}

	connect, resourceDomains, frame := m.UI.CSP.ConnectDomains, m.UI.CSP.ResourceDomains, m.UI.CSP.FrameDomains
	if len(connect) == 0 && len(resourceDomains) == 0 && len(frame) == 0 {
		var alias openaiWidgetCSPMeta
		if err := json.Unmarshal(meta, &alias); err == nil {
			connect, resourceDomains, frame = alias.WidgetCSP.ConnectDomains, alias.WidgetCSP.ResourceDomains, alias.WidgetCSP.FrameDomains
		}
	}

	res.CSP = protocol.UICSP{
		ConnectDomains:  filterHTTPSOrigins(connect, "connectDomains", serverID, uri),
		ResourceDomains: filterHTTPSOrigins(resourceDomains, "resourceDomains", serverID, uri),
		FrameDomains:    filterHTTPSOrigins(frame, "frameDomains", serverID, uri),
		BaseURIDomains:  filterHTTPSOrigins(m.UI.CSP.BaseURIDomains, "baseUriDomains", serverID, uri),
	}
	res.Permissions = protocol.UIPermissions{
		Camera:         hasPermissionKey(m.UI.Permissions, "camera"),
		Microphone:     hasPermissionKey(m.UI.Permissions, "microphone"),
		Geolocation:    hasPermissionKey(m.UI.Permissions, "geolocation"),
		ClipboardWrite: hasPermissionKey(m.UI.Permissions, "clipboardWrite"),
	}
	res.Domain = m.UI.Domain
	res.PrefersBorder = m.UI.PrefersBorder

	// 审计（apps_spec.mdx §Host Behavior "Audit Trail: Host SHOULD log CSP
	// configurations for security review"）。
	slog.Info("mcp apps: ui resource csp configured", "server", serverID, "uri", uri,
		"connect_domains", res.CSP.ConnectDomains, "resource_domains", res.CSP.ResourceDomains,
		"frame_domains", res.CSP.FrameDomains, "base_uri_domains", res.CSP.BaseURIDomains)
}

func hasPermissionKey(m map[string]json.RawMessage, key string) bool {
	_, ok := m[key]
	return ok
}

// filterHTTPSOrigins 丢弃非 https 源（含 "*." 通配子域校验），记 Warn 不中断——
// 单个非法域名不应让整份 UI 资源不可用（apps_spec.mdx §Host Behavior "No
// Loosening: Host MAY further restrict but MUST NOT allow undeclared domains"）。
func filterHTTPSOrigins(list []string, field, serverID, uri string) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, d := range list {
		if httpsOriginPattern.MatchString(d) {
			out = append(out, d)
		} else {
			slog.Warn("mcp apps: ui resource csp domain rejected (must be https origin)",
				"server", serverID, "uri", uri, "field", field, "domain", d)
		}
	}
	return out
}
