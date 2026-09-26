package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ElicitRequest MCP elicitation（form / url 模式）的宿主侧表示，供 Elicitor 实现
// （网关侧 broker，另一任务）据此渲染对话框或路由到 hook 引擎。
type ElicitRequest struct {
	ServerID, ServerName string
	SessionID            string // 取自 ctx 的 protocol.CtxSessionIDKey{}，可空
	Mode                 string // "form"（缺省视为 form）/ "url"
	Message              string
	RequestedSchema      json.RawMessage // form 模式
	URL                  string          // url 模式
	ElicitationID        string          // 旧纪元 url 模式的 elicitationId（新纪元已删除）
}

// ElicitResult 用户（或 hook）对 ElicitRequest 的答复。
type ElicitResult struct {
	Action  string         `json:"action"` // accept / decline / cancel
	Content map[string]any `json:"content,omitempty"`
}

// Elicitor 宿主的 elicitation 实现（网关侧 broker，另一任务实现；本任务只定义
// 接口并接线）。nil 表示宿主未配置 elicitation 能力：MCPManager 既不会声明
// elicitation 能力，也不会把 elicitation/create 转发给它。
type Elicitor interface {
	Elicit(ctx context.Context, req ElicitRequest) (ElicitResult, error)
}

// SetElicitor 注入宿主侧 elicitation 实现；atomic 存储，对之后连接的服务器生效
// （已连接的客户端在 initialize 时已固化 clientCapabilities，不会补声明）。
func (m *MCPManager) SetElicitor(e Elicitor) {
	m.elicitor.Store(&e)
}

// getElicitor 读取当前 Elicitor（可能为 nil）。
func (m *MCPManager) getElicitor() Elicitor {
	if p := m.elicitor.Load(); p != nil {
		return *p
	}
	return nil
}

// makeInputHandler 构造单个 MCP server 的统一服务端反向请求处理器：
// sampling/createMessage 复用 makeSamplingHandler（policy/预算检查不变）；roots/list 答空列表；
// elicitation/create 由本函数处理；其余方法未找到。旧纪元 handleServerRequest
// 与新纪元 MRTR（mcp_client_mrtr.go）两条路径共用同一个处理器实例，同一套校验与策略。
func (m *MCPManager) makeInputHandler(serverID, serverName string, trustTier int) ServerRequestHandler {
	sampling := m.makeSamplingHandler(serverName, trustTier)
	return func(ctx context.Context, method string, id int64, params json.RawMessage) (json.RawMessage, error) {
		switch method {
		case "sampling/createMessage":
			return sampling(ctx, method, id, params)
		case "roots/list":
			// Polaris 不向服务器暴露文件系统 roots（Roots 已弃用，SEP-2577）：答空列表而非报错，
			// 让未遵守能力声明的服务器仍能继续。
			return json.RawMessage(`{"roots":[]}`), nil
		case "elicitation/create":
			return m.handleElicitationCreate(ctx, serverID, serverName, params)
		default:
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("mcp: unsupported server method %q", method))
		}
	}
}

// handleElicitationCreate 解析并校验 elicitation/create 参数，构造 ElicitRequest 交给
// 已注入的 Elicitor。Elicitor 为 nil 时说明本客户端从未声明 elicitation 能力——正常
// 服务器不会发出该请求，但协议不保证服务器守规矩（尤其旧纪元的 server-initiated
// 请求没有 MRTR 层的能力声明前置校验），因此这里防御性地直接拒绝而非 panic 或误放行。
func (m *MCPManager) handleElicitationCreate(ctx context.Context, serverID, serverName string, params json.RawMessage) (json.RawMessage, error) {
	req, err := parseElicitRequest(ctx, serverID, serverName, params)
	if err != nil {
		return nil, err
	}
	elicitor := m.getElicitor()
	if elicitor == nil {
		return marshalElicitResult(&ElicitResult{Action: "decline"})
	}
	result, err := elicitor.Elicit(ctx, req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp: elicitation/create", err)
	}
	return marshalElicitResult(&result)
}

// marshalElicitResult 固定结构的内部序列化，失败即代码缺陷（wrapcheck 要求跨包错误
// 经 apperr 包装，即使这里实际上不可能失败）。
func marshalElicitResult(r *ElicitResult) (json.RawMessage, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp: marshal ElicitResult", err)
	}
	return b, nil
}

// elicitCreateParams elicitation/create 请求参数（elicitation §Protocol Messages）。
type elicitCreateParams struct {
	Mode            string          `json:"mode"`
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
	URL             string          `json:"url"`
	ElicitationID   string          `json:"elicitationId"`
}

// parseElicitRequest 解析并校验 elicitation/create 参数：mode 缺省为 form；form 模式
// 要求 requestedSchema 是受限子集（type:object + 平铺 primitive 属性）；url 模式要求
// https URL（elicitation §Safe URL Handling 4：SHOULD use HTTPS，本客户端收紧为 MUST，
// 拒绝明文 URL 泄露给用户点击）。
func parseElicitRequest(ctx context.Context, serverID, serverName string, raw json.RawMessage) (ElicitRequest, error) {
	var p elicitCreateParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return ElicitRequest{}, apperr.Wrap(apperr.CodeInvalidInput, "elicitation: invalid params", err)
	}
	mode := p.Mode
	if mode == "" {
		mode = "form" // mode 对 form 模式可选，缺省即 form（elicitation §Protocol Messages）
	}
	switch mode {
	case "form":
		if err := validateElicitFormSchema(p.RequestedSchema); err != nil {
			return ElicitRequest{}, err
		}
	case "url":
		if err := validateElicitURL(p.URL); err != nil {
			return ElicitRequest{}, err
		}
	default:
		return ElicitRequest{}, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: unsupported mode %q", mode))
	}
	sid, _ := ctx.Value(protocol.CtxSessionIDKey{}).(string)
	return ElicitRequest{
		ServerID: serverID, ServerName: serverName, SessionID: sid,
		Mode: mode, Message: p.Message, RequestedSchema: p.RequestedSchema,
		URL: p.URL, ElicitationID: p.ElicitationID,
	}, nil
}

// validateElicitURL 要求 url 模式的 URL 是带 host 的 https URL。
func validateElicitURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return apperr.New(apperr.CodeInvalidInput, "elicitation: url mode requires an https URL")
	}
	return nil
}

// elicitSchemaObject requestedSchema 顶层结构：必须是 type:object，属性为平铺 primitive。
type elicitSchemaObject struct {
	Type       string                     `json:"type"`
	Properties map[string]json.RawMessage `json:"properties"`
}

// validateElicitFormSchema 校验 form 模式 requestedSchema 是 elicitation §Requested Schema
// 定义的受限子集：顶层 type 必须是 object，每个属性必须是 primitive（string/number/
// integer/boolean）或 enum 数组（多选，items 为 primitive enum 或 anyOf/oneOf const 列表）。
// 拒绝嵌套 object、$ref、组合模式等——这些会让客户端无法生成简单表单，规范明确排除
// （"complex nested structures...are intentionally not supported"）。
func validateElicitFormSchema(raw json.RawMessage) error {
	if len(raw) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "elicitation: form mode requires requestedSchema")
	}
	var schema elicitSchemaObject
	if err := json.Unmarshal(raw, &schema); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "elicitation: invalid requestedSchema", err)
	}
	if schema.Type != "object" {
		return apperr.New(apperr.CodeInvalidInput, `elicitation: requestedSchema.type must be "object"`)
	}
	for name, propRaw := range schema.Properties {
		if err := validateElicitProperty(propRaw); err != nil {
			return apperr.Wrap(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: property %q", name), err)
		}
	}
	return nil
}

// elicitPropertySchema 单个属性的最小可判定形状（type + array 的 items）。
type elicitPropertySchema struct {
	Type  string          `json:"type"`
	Items json.RawMessage `json:"items"`
}

func validateElicitProperty(raw json.RawMessage) error {
	var p elicitPropertySchema
	if err := json.Unmarshal(raw, &p); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "invalid property schema", err)
	}
	switch p.Type {
	case "string", "number", "integer", "boolean":
		return nil
	case "array":
		return validateElicitArrayItems(p.Items)
	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("unsupported property type %q (nested objects are not allowed)", p.Type))
	}
}

// elicitArrayItems 多选 enum 的 items 形状：primitive+enum，或 anyOf/oneOf 的 const 列表
// （elicitation §Requested Schema「Multi-select enum」两种变体）。
type elicitArrayItems struct {
	Type  string `json:"type"`
	AnyOf []any  `json:"anyOf"`
	OneOf []any  `json:"oneOf"`
}

func validateElicitArrayItems(raw json.RawMessage) error {
	if len(raw) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "array property requires items")
	}
	var items elicitArrayItems
	if err := json.Unmarshal(raw, &items); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "invalid array items schema", err)
	}
	switch {
	case items.Type == "string" || items.Type == "number" || items.Type == "integer" || items.Type == "boolean":
		return nil
	case len(items.AnyOf) > 0 || len(items.OneOf) > 0:
		return nil
	default:
		return apperr.New(apperr.CodeInvalidInput, "array items must be a primitive enum or an anyOf/oneOf const list")
	}
}
