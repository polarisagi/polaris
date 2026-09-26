package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// PluginVarsResolver 为插件组件提供运行期变量（实现 mcp.PluginVarsResolver）。
type PluginVarsResolver struct {
	extRepo protocol.ExtensionRepository
	dataDir string
	config  *PluginConfigService
}

func NewPluginVarsResolver(extRepo protocol.ExtensionRepository, dataDir string, config *PluginConfigService) *PluginVarsResolver {
	return &PluginVarsResolver{extRepo: extRepo, dataDir: dataDir, config: config}
}

// PluginDataDir ${PLUGIN_DATA}：跨升级保留、卸载时删除的插件可写目录（Claude / Codex 同语义）。
func PluginDataDir(dataDir, pluginID string) string {
	return filepath.Join(dataDir, "extensions", "plugin-data", mcp.SanitizeToolNamePart(pluginID))
}

// ResolvePluginVars serverID 用于定位 channel 级配置作用域（插件子 MCP 行 ID 为
// "plugin_{pluginID}_{server}"）。必填 userConfig 缺失时拒绝：带着空 token 启动服务器
// 只会得到难以定位的鉴权失败（Claude 同样在配置完成前不启用）。
func (r *PluginVarsResolver) ResolvePluginVars(ctx context.Context, pluginID, serverID string) (pluginspec.Vars, error) {
	root, err := r.extRepo.GetPluginInstallPath(ctx, pluginID)
	if err != nil {
		return pluginspec.Vars{}, apperr.Wrap(apperr.CodeOf(err), "PluginVarsResolver", err)
	}
	data := PluginDataDir(r.dataDir, pluginID)
	// 两家均在首次引用时创建数据目录。
	if err := os.MkdirAll(data, 0o700); err != nil {
		return pluginspec.Vars{}, apperr.Wrap(apperr.CodeInternal, "PluginVarsResolver: create data dir", err)
	}
	vars := pluginspec.Vars{PluginRoot: root, PluginData: data}
	if r.config == nil {
		return vars, nil
	}
	scope := strings.TrimPrefix(serverID, "plugin_"+pluginID+"_")
	values, missing, err := r.config.ResolveStrings(ctx, pluginID, scope, true)
	if err != nil {
		return pluginspec.Vars{}, err
	}
	if len(missing) > 0 {
		return pluginspec.Vars{}, apperr.New(apperr.CodeInvalidInput,
			"plugin "+pluginID+" requires configuration: "+strings.Join(missing, ", "))
	}
	vars.UserConfig = values
	return vars, nil
}

// ResolveSkillRenderContext 插件技能渲染上下文（实现 skill.PluginRenderContextResolver）：
// 只提供非敏感 userConfig，敏感键列入 SensitiveKeys 由渲染器输出占位符（Claude 规则）；
// 技能渲染不要求必填项齐备（缺失项保留占位原文，由技能自身提示用户配置）。
func (r *PluginVarsResolver) ResolveSkillRenderContext(ctx context.Context, pluginID string) (pluginspec.RenderInput, error) {
	root, err := r.extRepo.GetPluginInstallPath(ctx, pluginID)
	if err != nil {
		return pluginspec.RenderInput{}, apperr.Wrap(apperr.CodeOf(err), "PluginVarsResolver.ResolveSkillRenderContext", err)
	}
	in := pluginspec.RenderInput{PluginRoot: root, PluginData: PluginDataDir(r.dataDir, pluginID)}
	if r.config == nil {
		return in, nil
	}
	values, _, err := r.config.ResolveStrings(ctx, pluginID, "", false)
	if err != nil {
		return pluginspec.RenderInput{}, err
	}
	schema, err := r.config.GetSchema(ctx, pluginID)
	if err != nil {
		return pluginspec.RenderInput{}, err
	}
	in.UserConfig, in.SensitiveKeys = values, map[string]bool{}
	for _, sc := range schema {
		for _, o := range sc.Options {
			if o.Sensitive {
				in.SensitiveKeys[o.Key] = true
			}
		}
	}
	return in, nil
}
