package lifecycle

import (
	"context"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// PluginVarsResolver 为插件组件提供运行期变量（实现 mcp.PluginVarsResolver）。
type PluginVarsResolver struct {
	extRepo protocol.ExtensionRepository
	dataDir string
}

func NewPluginVarsResolver(extRepo protocol.ExtensionRepository, dataDir string) *PluginVarsResolver {
	return &PluginVarsResolver{extRepo: extRepo, dataDir: dataDir}
}

// PluginDataDir ${PLUGIN_DATA}：跨升级保留、卸载时删除的插件可写目录（Claude / Codex 同语义）。
func PluginDataDir(dataDir, pluginID string) string {
	return filepath.Join(dataDir, "extensions", "plugin-data", mcp.SanitizeToolNamePart(pluginID))
}

func (r *PluginVarsResolver) ResolvePluginVars(ctx context.Context, pluginID string) (pluginspec.Vars, error) {
	root, err := r.extRepo.GetPluginInstallPath(ctx, pluginID)
	if err != nil {
		return pluginspec.Vars{}, apperr.Wrap(apperr.CodeOf(err), "PluginVarsResolver", err)
	}
	data := PluginDataDir(r.dataDir, pluginID)
	// 两家均在首次引用时创建数据目录。
	if err := os.MkdirAll(data, 0o700); err != nil {
		return pluginspec.Vars{}, apperr.Wrap(apperr.CodeInternal, "PluginVarsResolver: create data dir", err)
	}
	return pluginspec.Vars{PluginRoot: root, PluginData: data}, nil
}
