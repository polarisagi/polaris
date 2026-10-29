package pluginspec

import (
	"encoding/json"
)

// UnsupportedComponent 已解析但本宿主不激活的组件（agent-plugins 1.0 一致性条款 3：
// 忽略不支持的组件类型）。UI 展示为「此宿主不适用」，不得静默丢弃（ADR-0103 决策三）。
type UnsupportedComponent struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

const reasonNoEditorHost = "IDE/terminal host component; Polaris is a server-side agent without an editor or terminal surface"

// unsupportedKinds 组件类型 → (默认位置, 清单字段取值函数, 不适用原因)。
type unsupportedKind struct {
	kind     string
	defaults []string
	field    func(manifestWire) json.RawMessage
	reason   string
}

func unsupportedKinds() []unsupportedKind {
	return []unsupportedKind{
		{"lspServers", []string{".lsp.json"}, func(w manifestWire) json.RawMessage { return w.LSPServers }, reasonNoEditorHost},
		{"outputStyles", []string{"output-styles"}, func(w manifestWire) json.RawMessage { return w.OutputStyles }, reasonNoEditorHost},
		{"themes", []string{"themes"}, func(w manifestWire) json.RawMessage { return w.Themes }, reasonNoEditorHost},
		{"monitors", []string{"monitors/monitors.json"}, func(w manifestWire) json.RawMessage { return w.Monitors }, reasonNoEditorHost},
		{"workflows", []string{"workflows"}, func(w manifestWire) json.RawMessage { return w.Workflows }, reasonNoEditorHost},
		{"settings", []string{"settings.json"}, func(w manifestWire) json.RawMessage { return w.Settings },
			"applies Claude Code session settings (agent / subagentStatusLine) that have no Polaris equivalent"},
		{"experimental", nil, func(w manifestWire) json.RawMessage { return w.Experimental }, reasonNoEditorHost},
		// bin/ 在 Claude 中进入 Bash 工具 PATH：服务端宿主上等于允许插件替换任意命令名，
		// 不进入任何 PATH（ADR-0103 决策三）。
		{"bin", []string{"bin"}, nil, "executables are never added to PATH on a server host"},
	}
}

func detectUnsupported(root string, docs []manifestDoc) []UnsupportedComponent {
	var out []UnsupportedComponent
	for _, k := range unsupportedKinds() {
		if path, ok := firstExisting(root, k.defaults); ok {
			out = append(out, UnsupportedComponent{Kind: k.kind, Path: path, Reason: k.reason})
			continue
		}
		if k.field == nil {
			continue
		}
		if declared := docsWithField(docs, k.field); len(declared) > 0 {
			out = append(out, UnsupportedComponent{Kind: k.kind, Path: declared[0].path, Reason: k.reason})
		}
	}
	return out
}

func firstExisting(root string, rels []string) (string, bool) {
	for _, rel := range rels {
		if p, ok := existingFile(root, rel); ok {
			return p, true
		}
		if p, ok := existingDir(root, rel); ok {
			return p, true
		}
	}
	return "", false
}
