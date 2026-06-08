package native

import (
	"database/sql"
	"fmt"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/action"
	"github.com/polarisagi/polaris/pkg/action/tool"
	"github.com/polarisagi/polaris/pkg/extensions/marketplace"
	"github.com/polarisagi/polaris/pkg/extensions/mcp"
)

// RegisterExtensionTools 注册原生的 L2 扩展工具。
// 工具元数据从 builtin/<name>/tool.yaml + schema.json 文件加载。
func RegisterExtensionTools(
	sandbox *action.InProcessSandbox,
	toolReg *tool.InMemoryToolRegistry,
	mcpManager *mcp.MCPManager,
	db *sql.DB,
	marketplaceClient *marketplace.MCPMarketplaceClient,
	installMgr *marketplace.Manager,
	hitlGateway protocol.HITL,
) error {
	defs := []struct {
		name string
		fn   action.InProcessFn
	}{
		{"search_extension", MakeExtensionSearchFn(db, marketplaceClient)},
		{"install_extension", MakeExtensionInstallFn(db, marketplaceClient, installMgr, hitlGateway)},
	}

	for _, d := range defs {
		meta, err := LoadExtensionToolMeta(d.name)
		if err != nil {
			return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("extension_tools: load meta %q", d.name), err)
		}
		sandbox.Register(meta.Name, d.fn)
		if err := toolReg.Register(meta); err != nil {
			return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("extension_tools: register %q", d.name), err)
		}
	}

	return nil
}
