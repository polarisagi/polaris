package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	webui "github.com/polarisagi/polaris/web"
)

// ============================================================================
// MCP Apps 视图文档下发（ADR-0126）。
//
// 内层 View 不再经 srcdoc 加载：srcdoc 文档继承父文档 CSP，而沙箱代理页自身是
// default-src 'none' + 仅 sha256，两套策略取交集会拦掉 View 全部内联脚本/样式。
// 改为：主 API（鉴权）POST /v1/mcp-apps/views/{viewID}/document 在服务端构造完整文档
// 与该视图专属 CSP，换成一次性票据；沙箱监听器 GET /view?t=<token> 以 CSP 响应头
// 交付文档。CSP 由响应头而非 <meta> 承载，也不会被 View 自己的 HTML 篡改。
// ============================================================================

const (
	// mcpAppsViewTicketTTL 票据有效期：宿主拿到票据后立刻交给代理页加载，60s 足够，
	// 过长只会扩大票据被截获后的可用窗口。
	mcpAppsViewTicketTTL = 60 * time.Second
	// mcpAppsViewTicketMax 同时存活票据上限，防止鉴权后的客户端无界撑大内存。
	mcpAppsViewTicketMax = 256
	// mcpAppsViewTicketBytes 票据熵：24 字节 = 192 bit。
	mcpAppsViewTicketBytes = 24
)

// 票据指标 outcome 固定枚举（禁止放入 token 等高基数值）。
const (
	ticketOutcomeIssued  = "issued"
	ticketOutcomeHit     = "hit"
	ticketOutcomeMiss    = "miss"
	ticketOutcomeExpired = "expired"
	ticketOutcomeFull    = "full"
)

type mcpAppsViewTicket struct {
	doc     string
	csp     string
	expires time.Time
}

// mcpAppsViewTicketStore 一次性票据表。零值可用（map 惰性初始化），挂在 Server 上而非
// 包级变量（R1.3）；过期项在每次签发/取用时惰性清理，无后台协程。
type mcpAppsViewTicketStore struct {
	mu      sync.Mutex
	tickets map[string]mcpAppsViewTicket
	now     func() time.Time // 测试注入；nil = time.Now
}

func (st *mcpAppsViewTicketStore) clock() time.Time {
	if st.now != nil {
		return st.now()
	}
	return time.Now()
}

// issue 存入文档与 CSP，返回随机票据；表满（清理过期后仍满）返回 CodeResourceExhausted。
func (st *mcpAppsViewTicketStore) issue(doc, csp string) (string, error) {
	buf := make([]byte, mcpAppsViewTicketBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp apps: generate view ticket", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.clock()
	if st.tickets == nil {
		st.tickets = make(map[string]mcpAppsViewTicket)
	}
	for k, v := range st.tickets {
		if !now.Before(v.expires) {
			delete(st.tickets, k)
		}
	}
	if len(st.tickets) >= mcpAppsViewTicketMax {
		return "", apperr.New(apperr.CodeResourceExhausted, "mcp apps: too many pending view tickets")
	}
	st.tickets[token] = mcpAppsViewTicket{doc: doc, csp: csp, expires: now.Add(mcpAppsViewTicketTTL)}
	return token, nil
}

// take 取用即删。返回 outcome 供指标/日志区分：hit / miss（未知或已用）/ expired。
func (st *mcpAppsViewTicketStore) take(token string) (mcpAppsViewTicket, string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.tickets[token]
	if !ok {
		return mcpAppsViewTicket{}, ticketOutcomeMiss
	}
	delete(st.tickets, token)
	if !st.clock().Before(t.expires) {
		return mcpAppsViewTicket{}, ticketOutcomeExpired
	}
	return t, ticketOutcomeHit
}

// mcpAppsDomainRe 与沙箱页旧实现一致：仅 https 源，可带 "*." 子域通配与端口，
// 不允许路径/分号/空白，防止经域名字段注入额外 CSP 指令。
var mcpAppsDomainRe = regexp.MustCompile(`^https://(\*\.)?[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?(:[0-9]{1,5})?/?$`)

func sanitizeCSPDomains(list []string) []string {
	var out []string
	for _, d := range list {
		if mcpAppsDomainRe.MatchString(d) {
			out = append(out, d)
		}
	}
	return out
}

// buildMCPAppsCSP 按 apps_spec.mdx §Security Implications "CSP Construction from Metadata"
// 构造 View CSP（不含 frame-ancestors，由 /view 按请求 Host 追加）：connectDomains 缺省
// connect-src 'none'；resourceDomains 追加进 script/style/img/font/media-src；frameDomains
// 缺省 frame-src 'none'；baseUriDomains 缺省 base-uri 'self'；object-src 恒 'none'。
// 未声明的域一律不放开（No Loosening）。
func buildMCPAppsCSP(c protocol.UICSP) string {
	resource := sanitizeCSPDomains(c.ResourceDomains)
	connect := sanitizeCSPDomains(c.ConnectDomains)
	frame := sanitizeCSPDomains(c.FrameDomains)
	baseURI := sanitizeCSPDomains(c.BaseURIDomains)

	resourcePart := ""
	if len(resource) > 0 {
		resourcePart = " " + strings.Join(resource, " ")
	}
	connectSrc := "'none'"
	if len(connect) > 0 {
		connectSrc = "'self' " + strings.Join(connect, " ")
	}
	frameSrc := "'none'"
	if len(frame) > 0 {
		frameSrc = strings.Join(frame, " ")
	}
	baseURISrc := "'self'"
	if len(baseURI) > 0 {
		baseURISrc = strings.Join(baseURI, " ")
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src 'self' 'unsafe-inline'" + resourcePart,
		"style-src 'self' 'unsafe-inline'" + resourcePart,
		"img-src 'self' data:" + resourcePart,
		"font-src 'self'" + resourcePart,
		"media-src 'self' data:" + resourcePart,
		"connect-src " + connectSrc,
		"frame-src " + frameSrc,
		"object-src 'none'",
		"base-uri " + baseURISrc,
	}, "; ") + ";"
}

var (
	headOpenRe = regexp.MustCompile(`(?i)<head[^>]*>`)
	htmlOpenRe = regexp.MustCompile(`(?i)<html[^>]*>`)
)

// injectOpenAIShim 把 window.openai 垫片注入 View 文档 <head> 最前（先于 View 自身脚本）。
// View HTML 整体不可信，这里只做结构定位，不解析其语义。
func injectOpenAIShim(html string) string {
	injected := "<script>" + webui.MCPAppsOpenAIShim + "</script>"
	if loc := headOpenRe.FindStringIndex(html); loc != nil {
		return html[:loc[1]] + injected + html[loc[1]:]
	}
	if loc := htmlOpenRe.FindStringIndex(html); loc != nil {
		return html[:loc[1]] + "<head>" + injected + "</head>" + html[loc[1]:]
	}
	return injected + html
}

// handleMCPAppsViewDocument POST /v1/mcp-apps/views/{viewID}/document
// body: {"session_id":"..."}（插件设置视图不带 session_id，ADR-0125）。
func (s *Server) handleMCPAppsViewDocument(w http.ResponseWriter, r *http.Request) {
	if !s.appsSandboxCfg.Enabled || s.mcpMgr == nil {
		httputil.RespondError(w, "MCP Apps are unavailable", nil, http.StatusNotImplemented)
		return
	}
	viewID := r.PathValue("viewID")
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	view, err := s.authorizeMCPAppView(r.Context(), viewID, req.SessionID)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	res, err := s.mcpMgr.ReadUIResource(r.Context(), view.ServerID, view.ResourceURI)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	token, err := s.appsViewTickets.issue(injectOpenAIShim(res.HTML), buildMCPAppsCSP(res.CSP))
	if err != nil {
		metrics.RecordMCPAppViewTicket(r.Context(), ticketOutcomeFull)
		slog.Warn("mcp apps: view ticket issue failed", "view_id", viewID, "err", err)
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	metrics.RecordMCPAppViewTicket(r.Context(), ticketOutcomeIssued)
	slog.Info("mcp apps: view ticket issued", "view_id", viewID, "server", view.ServerID)
	// csp/permissions 沿用 Go 结构体默认序列化，前端 toWireCSP/toWirePermissions 原样消费
	// （用于 ui/initialize 的 hostCapabilities.sandbox）；html 不再下发给浏览器宿主。
	httputil.WriteJSON(w, map[string]any{
		"token":          token,
		"csp":            res.CSP,
		"permissions":    res.Permissions,
		"domain":         res.Domain,
		"prefers_border": res.PrefersBorder,
	})
}

// viewFrameAncestors View 文档的 frame-ancestors：CSP 对整条祖先链逐级校验，View 的祖先
// 既有沙箱代理页（沙箱源自身）也有最顶层宿主页，二者都必须放行；缺任一则浏览器拒绝渲染。
func (s *Server) viewFrameAncestors(requestHost string) string {
	self := s.appsSandboxCfg.SandboxOrigin
	if self == "" {
		self = "http://" + requestHost + " https://" + requestHost
	}
	return s.appsSandboxCfg.frameAncestors(requestHost) + " " + self
}

// handleMCPAppsViewPage GET /view?t=<token>（仅沙箱监听器）：一次性取用票据，以该视图
// 专属 CSP 响应头交付文档。无鉴权中间件——票据本身即能力（192 bit、60s、一次性）。
func (s *Server) handleMCPAppsViewPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("t")
	t, outcome := s.appsViewTickets.take(token)
	metrics.RecordMCPAppViewTicket(r.Context(), outcome)
	if outcome != ticketOutcomeHit {
		slog.Warn("mcp apps: view ticket rejected", "outcome", outcome)
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", t.csp+" frame-ancestors "+s.viewFrameAncestors(r.Host)+";")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if _, err := w.Write([]byte(t.doc)); err != nil {
		slog.Warn("mcp apps: view page write failed", "err", err)
	}
}
