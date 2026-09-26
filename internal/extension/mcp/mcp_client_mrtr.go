package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// methodToolsCall MCP 工具调用方法名（MRTR / Tasks / x-mcp-header 规则均以它为适用范围）。
const methodToolsCall = "tools/call"

// maxMRTRRounds MRTR（Multi Round-Trip Requests）单个客户端请求最多允许的往返轮数
// （2026-07-28 basic/patterns/mrtr）。规范未规定上限，服务器理论上可以无限次要求更多
// 输入（"Servers MUST NOT assume that clients will fulfill..."），客户端必须自行设界，
// 否则一个恶意/故障服务器能让单次 methodToolsCall 无限挂起。
const maxMRTRRounds = 8

// mrtrInputRequest InputRequiredResult.inputRequests 的单个条目（server 分配的字符串
// key → {method, params}），method 取值限于 elicitation/create、sampling/createMessage、
// roots/list（mrtr §InputRequests）。
type mrtrInputRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// mrtrResult methodToolsCall / resources/read / prompts/get 结果信封的公共前缀。旧纪元结果
// 没有 resultType 字段，零值走 complete 分支（规范要求：缺省视为 complete）。
type mrtrResult struct {
	ResultType    string                      `json:"resultType"`
	InputRequests map[string]mrtrInputRequest `json:"inputRequests,omitempty"`
	RequestState  json.RawMessage             `json:"requestState,omitempty"`
}

// request 发起一次可能触发 MRTR 的客户端请求（仅 methodToolsCall / resources/read /
// prompts/get 三种受支持，mrtr §Supported Requests）。新纪元下解析 resultType：
// 收到 "input_required" 时构造 inputResponses 并携带 requestState 原样重试，直到
// 服务器返回最终结果或轮数耗尽。旧纪元没有 MRTR，一次往返即完成。
func (c *MCPClient) request(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	current := params
	for round := 0; round < maxMRTRRounds; round++ {
		result, err := c.callWithHeaderMismatchRetry(ctx, method, current)
		if err != nil {
			return nil, err
		}
		if c.protocolEra() != eraModern {
			return result, nil
		}
		var probe mrtrResult
		if err := json.Unmarshal(result, &probe); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "mcp: parse result envelope", err)
		}
		if probe.ResultType != "input_required" {
			return result, nil
		}
		next, err := c.fulfillInputRequired(ctx, current, probe)
		if err != nil {
			return nil, err
		}
		current = next
	}
	return nil, apperr.New(apperr.CodeResourceExhausted,
		fmt.Sprintf("mcp: %s exceeded %d MRTR rounds without completing", method, maxMRTRRounds))
}

// callWithHeaderMismatchRetry methodToolsCall 收到 errCodeHeaderMismatch 时（Streamable HTTP
// 的 x-mcp-header 标注与服务器当前 tools/list 不一致）刷新 toolHeaders 缓存后重试一次
// （2026-07-28 streamable-http §Custom Headers，SHOULD）；其余请求/错误原样透传。
func (c *MCPClient) callWithHeaderMismatchRetry(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	result, err := c.call(ctx, method, params)
	if err == nil || method != methodToolsCall {
		return result, err
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != errCodeHeaderMismatch {
		return result, err
	}
	if _, lerr := c.ListTools(ctx); lerr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp: refresh tool headers after header mismatch", lerr)
	}
	return c.call(ctx, method, params)
}

// fulfillInputRequired 处理一次 InputRequiredResult：为其中的 inputRequests 逐一调用
// 统一输入处理器得到 inputResponses，并按规范把 requestState 原样（或缺省即不带）
// 合并进原始参数的克隆，供下一轮重试。
func (c *MCPClient) fulfillInputRequired(ctx context.Context, orig map[string]any, res mrtrResult) (map[string]any, error) {
	if len(res.InputRequests) == 0 && len(res.RequestState) == 0 {
		return nil, apperr.New(apperr.CodeInvalidInput,
			"mcp: input_required result must include inputRequests and/or requestState")
	}
	next := make(map[string]any, len(orig)+2)
	for k, v := range orig {
		next[k] = v
	}
	if len(res.InputRequests) > 0 {
		responses, err := c.fulfillInputRequests(ctx, res.InputRequests)
		if err != nil {
			return nil, err
		}
		next["inputResponses"] = responses
	} else {
		delete(next, "inputResponses") // 本轮无新请求，不携带上一轮的陈旧答复
	}
	if len(res.RequestState) > 0 {
		next["requestState"] = res.RequestState
	} else {
		delete(next, "requestState") // client requirement 2：服务器未回传即不得携带
	}
	return next, nil
}

// fulfillInputRequests 对 inputRequests 中每个条目调用统一输入处理器（sampling/
// elicitation/roots 合一，由 SetInputHandler 装配），拒绝客户端未声明能力的方法
// （mrtr §Server Requirements 7：客户端必须防御服务器请求未声明的能力）。
func (c *MCPClient) fulfillInputRequests(ctx context.Context, reqs map[string]mrtrInputRequest) (map[string]json.RawMessage, error) {
	c.mu.Lock()
	handler := c.serverReqHandler
	c.mu.Unlock()
	responses := make(map[string]json.RawMessage, len(reqs))
	for key, req := range reqs {
		if !c.declaresCapability(req.Method) {
			return nil, apperr.New(apperr.CodeForbidden,
				fmt.Sprintf("mcp: server requested undeclared client capability %q (input request %q)", req.Method, key))
		}
		if handler == nil {
			return nil, apperr.New(apperr.CodeInternal, "mcp: no input handler configured for "+req.Method)
		}
		result, err := handler(ctx, req.Method, 0, req.Params)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "mcp: fulfill input request "+key, err)
		}
		responses[key] = result
	}
	return responses, nil
}
