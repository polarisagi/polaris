package pluginspec

import (
	"encoding/json"
	"regexp"
	"strings"
)

const (
	RuleSetupToolShape     = "polaris.setup.shape"
	RuleSetupStatusOrphan  = "polaris.setup.statusWithoutSetup"
	RuleDefaultPromptShape = "codex.interface.defaultPrompt"

	// maxDefaultPrompts Codex interface.defaultPrompt 的条数上限。
	maxDefaultPrompts = 3
)

// setupToolNamePattern 插件内裸工具名：MCP 工具名允许 '.'，注册进 registry 时由 SanitizeToolNamePart 归一化。
var setupToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// loadSetupTools 从 extensions["ai.polarisagi"] 取 setupTool/statusTool（ADR-0103：私有字段只进厂商命名空间）。
// 多份清单按优先级取第一个非空值；statusTool 缺少 setupTool 时没有"设置"可查询状态，忽略并出诊断。
// 命名空间对象本身的格式错误已由 loadScheduledTasks 报告，这里静默跳过以免重复诊断。
func loadSetupTools(docs []manifestDoc, ds *diagnostics) (setup, status string) {
	for _, doc := range docs {
		rawExt, ok := doc.wire.Extensions[polarisNamespaceKey]
		if !ok || len(rawExt) == 0 || string(rawExt) == "null" {
			continue
		}
		var ext struct {
			SetupTool  json.RawMessage `json:"setupTool"`
			StatusTool json.RawMessage `json:"statusTool"`
		}
		if json.Unmarshal(rawExt, &ext) != nil {
			continue
		}
		if setup == "" {
			setup = parseSetupToolName(ext.SetupTool, "setupTool", doc.path, ds)
		}
		if status == "" {
			status = parseSetupToolName(ext.StatusTool, "statusTool", doc.path, ds)
		}
	}
	if status != "" && setup == "" {
		ds.warnf("manifest", firstDocPath(docs), RuleSetupStatusOrphan,
			"statusTool %q ignored: it requires setupTool", status)
		status = ""
	}
	return setup, status
}

func firstDocPath(docs []manifestDoc) string {
	if len(docs) == 0 {
		return ""
	}
	return docs[0].path
}

func parseSetupToolName(raw json.RawMessage, field, path string, ds *diagnostics) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil || !setupToolNamePattern.MatchString(name) {
		ds.warnf("manifest", path, RuleSetupToolShape,
			"extensions[%q].%s must be a tool name matching %s, ignored", polarisNamespaceKey, field, setupToolNamePattern)
		return ""
	}
	return name
}

// normalizeDefaultPrompts 把 interface.defaultPrompt（字符串或字符串数组）归一为去空白、去空串、≤3 条的列表。
// 超出上限截断而非整体拒绝：多余的提示只影响展示，不应让整个 interface 块失效。
func normalizeDefaultPrompts(iface *CodexInterface, path string, ds *diagnostics) []string {
	if iface == nil || len(iface.DefaultPrompt) == 0 || string(iface.DefaultPrompt) == "null" {
		return nil
	}
	var items []string
	var single string
	if err := json.Unmarshal(iface.DefaultPrompt, &single); err == nil {
		items = []string{single}
	} else if err := json.Unmarshal(iface.DefaultPrompt, &items); err != nil {
		ds.warnf("manifest", path, RuleDefaultPromptShape,
			"interface.defaultPrompt must be a string or an array of strings, ignored")
		return nil
	}
	out := make([]string, 0, maxDefaultPrompts)
	for _, it := range items {
		if t := strings.TrimSpace(it); t != "" {
			out = append(out, t)
		}
	}
	if len(out) > maxDefaultPrompts {
		ds.warnf("manifest", path, RuleDefaultPromptShape,
			"interface.defaultPrompt has %d entries, keeping the first %d", len(out), maxDefaultPrompts)
		out = out[:maxDefaultPrompts]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
