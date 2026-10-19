package codeact

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
