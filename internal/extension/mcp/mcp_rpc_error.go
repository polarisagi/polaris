package mcp

import (
	"encoding/json"
	"fmt"
)

// MCP 2026-07-28 规范保留的错误码（-32020~-32099，basic/index#error-codes）。
const (
	errCodeHeaderMismatch             = -32020
	errCodeMissingRequiredClientCap   = -32021
	errCodeUnsupportedProtocolVersion = -32022
	errCodeMethodNotFound             = -32601
)

// RPCError JSON-RPC 错误响应（保留 code/data 供版本协商与回退判断）。
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message) }

// isModernError 可识别的新纪元（2026-07-28+）协议错误：据此判定服务器为新纪元，不得回退 initialize。
func (e *RPCError) isModernError() bool {
	switch e.Code {
	case errCodeHeaderMismatch, errCodeMissingRequiredClientCap, errCodeUnsupportedProtocolVersion:
		return true
	}
	return false
}

// supportedVersions UnsupportedProtocolVersionError.data.supported。
func (e *RPCError) supportedVersions() []string {
	var d struct {
		Supported []string `json:"supported"`
	}
	if e.Code != errCodeUnsupportedProtocolVersion || json.Unmarshal(e.Data, &d) != nil {
		return nil
	}
	return d.Supported
}
