package pluginspec

import (
	"encoding/json"
	"strings"
)

const RuleMCPShape = "mcp.manifest-shape"

// loadMCPServers 默认文件先加载（.mcp.json：Claude/Codex；mcp.json：agent-plugins），
// 再按清单优先级合并声明；同名后声明覆盖（Claude 合并语义）。
func loadMCPServers(root string, docs []manifestDoc, opts LoadOptions, ds *diagnostics) ([]MCPServer, []UserConfigOption) {
	set := mcpServerSet{remote: opts.RemoteBundles}
	for _, name := range []string{".mcp.json", "mcp.json"} {
		if f, ok := existingFile(root, name); ok {
			parseMCPFile(f, &set, ds)
		}
	}
	for _, doc := range docsWithField(docs, func(w manifestWire) json.RawMessage { return w.MCPServers }) {
		for _, item := range mcpDeclarations(doc.wire.MCPServers) {
			applyMCPDeclaration(root, doc.path, item, &set, ds)
		}
	}
	return set.list(), set.bundleUserConfig
}

// mcpDeclarations 把 "路径 / 内联映射 / 二者混合数组" 统一展开为声明项列表。
func mcpDeclarations(raw json.RawMessage) []json.RawMessage {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		return items
	}
	return []json.RawMessage{raw}
}

func applyMCPDeclaration(root, manifestPath string, item json.RawMessage, set *mcpServerSet, ds *diagnostics) {
	var ref string
	if json.Unmarshal(item, &ref) == nil {
		applyMCPReference(root, manifestPath, ref, set, ds)
		return
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(item, &servers); err != nil {
		ds.errorf("mcp", manifestPath, RuleMCPShape, "mcpServers entry must be a path, bundle or server map")
		return
	}
	parseMCPServerMap(servers, manifestPath, set, ds)
}

// applyMCPReference 处理字符串形态：.json 配置文件、.mcpb/.dxt 本地包、https 包地址。
func applyMCPReference(root, manifestPath, ref string, set *mcpServerSet, ds *diagnostics) {
	lower := strings.ToLower(ref)
	isBundle := strings.HasSuffix(lower, ".mcpb") || strings.HasSuffix(lower, ".dxt")
	switch {
	case strings.HasPrefix(lower, "https://"):
		if !isBundle {
			ds.errorf("mcp", manifestPath, RuleMCPShape, "remote mcpServers reference %q must end in .mcpb or .dxt", ref)
			return
		}
		// 解析期不发起网络请求（本包无出站能力，XR-06）：安装层先经 SafeDialer 下载
		// （ListRemoteBundles 给出清单），再通过 LoadOptions.RemoteBundles 提供本地路径。
		local, fetched := set.remote[ref]
		if !fetched {
			ds.errorf("mcp", manifestPath, RuleMCPBundleRemote, "remote bundle %q was not fetched by the installer", ref)
			return
		}
		set.addBundle(root, local, ds)
	case isBundle:
		abs, rule, err := resolveComponentPath(root, ref, false)
		if err != nil {
			ds.errorf("mcp", manifestPath, rule, "bundle %q: %v", ref, err)
			return
		}
		set.addBundle(root, abs, ds)
	default:
		abs, rule, err := resolveComponentPath(root, ref, false)
		if err != nil {
			ds.errorf("mcp", manifestPath, rule, "mcpServers path %q: %v", ref, err)
			return
		}
		parseMCPFile(abs, set, ds)
	}
}

func (s *mcpServerSet) addBundle(root, bundlePath string, ds *diagnostics) {
	res, ok := loadMCPBundle(root, bundlePath, ds)
	if !ok {
		return
	}
	s.put(res.server)
	s.bundleUserConfig = append(s.bundleUserConfig, res.userConfig...)
}

// ListRemoteBundles 列出清单中声明的 https .mcpb/.dxt 地址，供安装层预先下载。
func ListRemoteBundles(root string) []string {
	var ds diagnostics
	var out []string
	for _, doc := range readManifests(root, &ds) {
		if len(doc.wire.MCPServers) == 0 {
			continue
		}
		for _, item := range mcpDeclarations(doc.wire.MCPServers) {
			var ref string
			if json.Unmarshal(item, &ref) == nil && strings.HasPrefix(strings.ToLower(ref), "https://") {
				out = append(out, ref)
			}
		}
	}
	return out
}

// ListMCPServersInDir 读取目录下的默认 MCP 配置（.mcp.json 与 mcp.json），供独立连接器安装使用。
func ListMCPServersInDir(dir string) ([]MCPServer, []Diagnostic) {
	var ds diagnostics
	var set mcpServerSet
	for _, name := range []string{".mcp.json", "mcp.json"} {
		if f, ok := existingFile(dir, name); ok {
			parseMCPFile(f, &set, &ds)
		}
	}
	return set.list(), ds
}
