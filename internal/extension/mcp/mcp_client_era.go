package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/version"
)

// MCP 协议纪元（2026-07-28 basic/versioning）：新纪元无握手、每个请求在 _meta 中携带版本与能力；
// 旧纪元以 initialize 建立会话。Polaris 为双纪元客户端：先以 server/discover 探测，失败再回退。
const (
	modernProtocolVersion = "2026-07-28"
	discoverProbeTimeout  = 5 * time.Second

	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

type protocolEra int32

const (
	eraUnknown protocolEra = iota
	eraModern
	eraLegacy
)

// Initialize 确定服务器纪元并完成版本协商：
//   - HTTP+SSE（已弃用传输）只可能是旧纪元，直接 initialize；
//   - 其余先发 server/discover：成功即新纪元；返回可识别的新纪元错误（如版本不支持）按其
//     supported 列表选择，不回退；其他错误或超时视为旧纪元，回退 initialize（stdio 规则：回退不得绑定特定错误码）。
func (c *MCPClient) Initialize(ctx context.Context) error {
	if c.cfg.Transport == MCPSSE {
		return c.initializeLegacy(ctx)
	}
	err := c.discover(ctx)
	if err == nil {
		return nil
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) && rpcErr.isModernError() {
		if slices.Contains(rpcErr.supportedVersions(), mcpProtocolVersion) {
			return c.initializeLegacy(ctx)
		}
		return apperr.Wrap(apperr.CodeUnimplemented, "mcp: server supports no protocol version this client speaks", err)
	}
	slog.Debug("mcp: server/discover probe failed, falling back to initialize", "server", c.cfg.ServerName, "err", err)
	return c.initializeLegacy(ctx)
}

type discoverResult struct {
	SupportedVersions []string `json:"supportedVersions"`
	Capabilities      struct {
		Experimental map[string]json.RawMessage `json:"experimental"`
		Extensions   map[string]json.RawMessage `json:"extensions"`
	} `json:"capabilities"`
	Instructions string `json:"instructions"`
}

func (c *MCPClient) discover(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, discoverProbeTimeout)
	defer cancel()
	result, err := c.call(probeCtx, "server/discover", c.withMeta(nil, modernProtocolVersion))
	if err != nil {
		return err
	}
	var d discoverResult
	if err := json.Unmarshal(result, &d); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "mcp: server/discover result", err)
	}
	if len(d.SupportedVersions) == 0 {
		// 不是合法 DiscoverResult：旧纪元服务器未校验方法名、按旧语义作答，视为旧纪元。
		return apperr.New(apperr.CodeInvalidInput, "mcp: server/discover returned no supportedVersions")
	}
	if !slices.Contains(d.SupportedVersions, modernProtocolVersion) {
		// 新纪元服务器但不支持本客户端的新纪元版本：若支持旧版本，交由调用方回退。
		return &RPCError{Code: errCodeUnsupportedProtocolVersion, Message: "no mutually supported modern version",
			Data: mustJSON(map[string]any{"supported": d.SupportedVersions})}
	}
	c.era.Store(int32(eraModern))
	c.serverMeta.Store(&ServerMeta{Instructions: d.Instructions, Experimental: d.Capabilities.Experimental,
		Extensions: d.Capabilities.Extensions})
	return nil
}

func (c *MCPClient) protocolEra() protocolEra { return protocolEra(c.era.Load()) }

// protocolVersion 当前请求应携带的版本（新纪元 2026-07-28，旧纪元 2025-11-25）。
func (c *MCPClient) protocolVersion() string {
	if c.protocolEra() == eraModern {
		return modernProtocolVersion
	}
	return mcpProtocolVersion
}

// withMeta 在请求参数中合并新纪元必需的 _meta（版本、客户端身份、与本请求相关的客户端能力）。
func (c *MCPClient) withMeta(params any, protoVersion string) map[string]any {
	m := map[string]any{}
	if params != nil {
		raw, err := json.Marshal(params)
		if err == nil {
			err = json.Unmarshal(raw, &m)
		}
		if err != nil {
			// 非对象参数无法携带 _meta：按空参数发送并留痕（请求会被服务器按缺参拒绝，而非静默错参）。
			slog.Warn("mcp: request params are not an object, sending _meta only", "server", c.cfg.ServerName, "err", err)
			m = map[string]any{}
		}
	}
	meta, _ := m["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[metaProtocolVersion] = protoVersion
	meta[metaClientInfo] = map[string]string{"name": "polaris", "version": version.Version}
	meta[metaClientCapabilities] = c.clientCapabilities()
	m["_meta"] = meta
	return m
}

// clientCapabilities 客户端能力：只声明实际能处理的输入请求类型（MRTR：服务器不得请求未声明的能力）。
func (c *MCPClient) clientCapabilities() map[string]any {
	caps := map[string]any{}
	c.mu.Lock()
	hasSampling := c.serverReqHandler != nil
	c.mu.Unlock()
	if hasSampling {
		caps["sampling"] = map[string]any{}
	}
	return caps
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v) //nolint:errchkjson // 仅用于内部固定结构
	return b
}
