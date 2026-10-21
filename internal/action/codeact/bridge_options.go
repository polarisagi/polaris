package codeact

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// CallAuthorizer 桥内每次工具调用的统一权限判定（ADR-0121 决策六；调用方定义，HE-3）。
// ok=true 时 grant 是该次调用的授权凭据，桥会覆盖写入 ctx；ok=false 时 reason 回给脚本。
type CallAuthorizer interface {
	AuthorizeCall(ctx context.Context, tool types.Tool, args []byte, taint types.TaintLevel) (grant protocol.PermissionGrant, reason string, ok bool)
}

// ProfileChecker 校验当前 Agent profile 是否允许调用指定工具（R2-15）。
type ProfileChecker interface {
	CheckProfileTool(toolName, delegateTarget string) error
}

// BridgeOption 工具桥启动配置选项。
type BridgeOption func(*bridgeConfig)

type bridgeConfig struct {
	dataDir        string
	token          string
	profileChecker ProfileChecker
	authorizer     CallAuthorizer
}

// WithBridgeCallAuthorizer 注入桥内调用授权器；未注入时桥 fail-closed 退回只读幂等规则。
func WithBridgeCallAuthorizer(a CallAuthorizer) BridgeOption {
	return func(cfg *bridgeConfig) { cfg.authorizer = a }
}

// WithBridgeDataDir 指定 Bridge Unix Socket 根数据目录（socket 写入 <dataDir>/run/）。
func WithBridgeDataDir(dir string) BridgeOption {
	return func(cfg *bridgeConfig) { cfg.dataDir = dir }
}

// WithBridgeToken 指定 Bridge 鉴权令牌。若为空则自动生成随机 UUID。
func WithBridgeToken(tok string) BridgeOption {
	return func(cfg *bridgeConfig) { cfg.token = tok }
}

// WithBridgeProfileChecker 注入 Agent 角色工具校验器。
func WithBridgeProfileChecker(checker ProfileChecker) BridgeOption {
	return func(cfg *bridgeConfig) { cfg.profileChecker = checker }
}

func parseBridgeConfig(opts []BridgeOption) bridgeConfig {
	var cfg bridgeConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}
