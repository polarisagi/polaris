package pluginspec

import (
	"encoding/json"

	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	RuleEntryConflict = "claude.marketplace.strict"
	RuleEntryHooks    = "claude.marketplace.entry_hooks"
)

// entryDocPath 诊断中标识"来自市场条目"的组件来源。
const entryDocPath = "marketplace entry"

// applyMarketplaceEntry 市场条目与插件自身清单的组合（Claude marketplace reference「How an entry
// combines with plugin.json」/ Strict mode）：
//   - 插件无清单：条目即清单（全部 plugin.json 字段生效）；
//   - 有清单且 strict（默认）：条目的 commands/agents/skills/hooks/outputStyles/themes 追加到清单，
//     hooks 按事件替换清单同名事件；条目的 mcpServers/lspServers/userConfig/channels 不生效；
//   - 有清单且 strict:false：条目声明任一组件字段即冲突，插件不加载。
func applyMarketplaceEntry(docs []manifestDoc, entry *MarketplaceEntry, ds *diagnostics) ([]manifestDoc, error) {
	if entry == nil || len(entry.Manifest) == 0 {
		return docs, nil
	}
	var w manifestWire
	if err := json.Unmarshal(entry.Manifest, &w); err != nil {
		return docs, apperr.Wrap(apperr.CodeInvalidInput, "pluginspec: marketplace entry", err)
	}
	w.Name = entry.Name
	if len(w.Hooks) > 0 && w.Hooks[0] != '{' {
		ds.errorf("hooks", entryDocPath, RuleEntryHooks, "hooks as a file path or array are not yet supported in a marketplace entry")
		w.Hooks = nil
	}
	if len(docs) == 0 {
		return []manifestDoc{{format: FormatClaude, path: entryDocPath, wire: w}}, nil
	}
	components := manifestWire{Commands: w.Commands, Agents: w.Agents, Skills: w.Skills, Hooks: w.Hooks,
		OutputStyles: w.OutputStyles, Themes: w.Themes}
	hasComponents := len(components.Commands)+len(components.Agents)+len(components.Skills)+len(components.Hooks)+
		len(components.OutputStyles)+len(components.Themes) > 0
	if !hasComponents {
		return docs, nil
	}
	if !entry.Strict {
		return docs, apperr.New(apperr.CodeInvalidInput,
			"Plugin "+entry.Name+" has conflicting manifests: both plugin.json and marketplace entry specify components")
	}
	return append(docs, manifestDoc{format: FormatClaude, path: entryDocPath, wire: components}), nil
}

// applyEntryDisplay 条目设置的展示字段与 defaultEnabled 优先于插件清单（Claude 规则）。
func applyEntryDisplay(p *Plugin, entry *MarketplaceEntry) {
	if entry == nil {
		return
	}
	p.DisplayName = firstNonEmpty(entry.DisplayName, p.DisplayName)
	p.Description = firstNonEmpty(entry.Description, p.Description)
	p.Homepage = firstNonEmpty(entry.Homepage, p.Homepage)
	if entry.Author != nil {
		p.Author = entry.Author
	}
	if len(entry.Keywords) > 0 {
		p.Keywords = entry.Keywords
	}
	if entry.DefaultEnabled != nil {
		p.DefaultEnabled = *entry.DefaultEnabled
	}
	if len(p.Dependencies) == 0 {
		p.Dependencies = entry.Dependencies
	}
}

// replaceEntryHookEvents 条目 hooks 的匹配组按事件替换清单同名事件（strict 追加规则的例外）。
func replaceEntryHookEvents(sources []HookSource) []HookSource {
	var entryEvents map[string]json.RawMessage
	for _, s := range sources {
		if s.Source == entryDocPath+"#hooks" {
			entryEvents = s.Events
		}
	}
	if entryEvents == nil {
		return sources
	}
	out := sources[:0]
	for _, s := range sources {
		if s.Source != entryDocPath+"#hooks" {
			for ev := range entryEvents {
				delete(s.Events, ev)
			}
			if len(s.Events) == 0 {
				continue
			}
			s.Digest = hookDigest(s.Events)
		}
		out = append(out, s)
	}
	return out
}
