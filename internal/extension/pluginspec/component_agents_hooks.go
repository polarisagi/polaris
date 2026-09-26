package pluginspec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const (
	RuleAgentShape = "claude.agents.shape"
	RuleAgentParse = "claude.agent.parse"
	RuleHooksShape = "hooks.shape"
)

// AgentFile 子 Agent 定义（Claude agents/*.md 或 Codex agents/*.toml，见 agent.go）。结构化
// 常用字段，其余原样保留在 Fields；NotApplied 列出已解析但本宿主不生效的字段（UI 展示）。
type AgentFile struct {
	Name            string         `json:"name"` // 缺省取相对 agents/ 的路径，子目录以 ":" 连接
	Description     string         `json:"description"`
	Format          string         `json:"format"` // claude / codex
	File            string         `json:"file"`
	Tools           []string       `json:"tools,omitempty"`
	DisallowedTools []string       `json:"disallowed_tools,omitempty"`
	Model           string         `json:"model,omitempty"`
	Skills          []string       `json:"skills,omitempty"`
	MaxTurns        int            `json:"max_turns,omitempty"`
	ReadOnly        bool           `json:"read_only,omitempty"`
	Nicknames       []string       `json:"nicknames,omitempty"`
	NotApplied      []string       `json:"not_applied,omitempty"`
	Fields          map[string]any `json:"fields,omitempty"`
	Body            string         `json:"-"` // Claude 正文 / Codex developer_instructions
}

// loadAgents 默认 agents/（递归，子目录计入名称）；Claude 清单 agents 替换默认，且只接受 .md 文件。
func loadAgents(root string, docs []manifestDoc, ds *diagnostics) []AgentFile {
	declared := docsWithField(docs, func(w manifestWire) json.RawMessage { return w.Agents })
	if len(declared) == 0 {
		dir, ok := existingDir(root, "agents")
		if !ok {
			return nil
		}
		var out []AgentFile
		for _, f := range markdownFilesUnder(dir, ds) {
			out = appendAgent(out, f, commandName(dir, f), true, ds)
		}
		return out
	}
	var out []AgentFile
	for _, doc := range declared {
		paths, ok := rawPaths(doc.wire.Agents)
		if !ok {
			ds.errorf("agent", doc.path, RuleAgentShape, "agents must be a path or an array of .md paths")
			continue
		}
		for _, rel := range paths {
			abs, rule, err := resolveComponentPath(root, rel, false)
			if err != nil {
				ds.errorf("agent", doc.path, rule, "agents path %q: %v", rel, err)
				continue
			}
			if !strings.EqualFold(filepath.Ext(abs), ".md") {
				ds.errorf("agent", doc.path, RuleAgentShape, "agents entry %q must be a .md file (directories are not accepted)", rel)
				continue
			}
			out = appendAgent(out, abs, strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs)), true, ds)
		}
	}
	return out
}

func appendAgent(out []AgentFile, file, defaultName string, inPlugin bool, ds *diagnostics) []AgentFile {
	raw, err := os.ReadFile(file)
	if err != nil {
		ds.errorf("agent", file, RuleAgentParse, "read failed: %v", err)
		return out
	}
	fields, body, err := splitFrontmatter(raw)
	if err != nil {
		ds.errorf("agent", file, RuleAgentParse, "%v", err)
		return out
	}
	if fields == nil {
		fields = map[string]any{}
	}
	a := AgentFile{Format: AgentFormatClaude, File: file, Fields: fields, Body: body}
	a.Name, _ = fmString(fields, "name")
	a.Name = firstNonEmpty(strings.TrimSpace(a.Name), defaultName)
	a.Description, _ = fmString(fields, "description")
	if !validSkillName(a.Name) {
		ds.errorf("agent", file, RuleAgentParse, "agent name %q is invalid", a.Name)
		return out
	}
	a.applyClaudeAgentFields(inPlugin, ds)
	return append(out, a)
}

func markdownFilesUnder(dir string, ds *diagnostics) []string {
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		ds.errorf("agent", dir, RuleSkillScanFailed, "scan failed: %v", err)
		return nil
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		switch {
		case strings.HasPrefix(e.Name(), "."):
		case e.IsDir():
			out = append(out, markdownFilesUnder(full, ds)...)
		case strings.EqualFold(filepath.Ext(e.Name()), ".md"):
			out = append(out, full)
		}
	}
	return out
}

// HookSource 一份 hooks 配置（文件或清单内联）。Events 为 事件名→匹配组数组 的原始 JSON，
// 运行期模型与校验在 internal/action/hook（ADR-0103 决策六）。
// Digest 是定义内容哈希：插件 hooks 按哈希审阅信任，定义变更即回到待审（安装≠信任）。
type HookSource struct {
	Source string                     `json:"source"`
	Events map[string]json.RawMessage `json:"events"`
	Digest string                     `json:"digest"`
}

// loadHooks 默认 hooks/hooks.json 与清单声明合并（两家语义一致）；Claude 清单 hooks 可为
// 路径、内联对象或二者混合数组，Codex 为路径或路径数组。
func loadHooks(root string, docs []manifestDoc, ds *diagnostics) []HookSource {
	var out []HookSource
	seen := map[string]bool{}
	if f, ok := existingFile(root, "hooks", "hooks.json"); ok {
		out = appendHookFile(out, f, seen, ds)
	}
	for _, doc := range docsWithField(docs, func(w manifestWire) json.RawMessage { return w.Hooks }) {
		var items []json.RawMessage
		if err := json.Unmarshal(doc.wire.Hooks, &items); err != nil {
			items = []json.RawMessage{doc.wire.Hooks}
		}
		for _, item := range items {
			var rel string
			if json.Unmarshal(item, &rel) == nil {
				abs, rule, err := resolveComponentPath(root, rel, false)
				if err != nil {
					ds.errorf("hooks", doc.path, rule, "hooks path %q: %v", rel, err)
					continue
				}
				out = appendHookFile(out, abs, seen, ds)
				continue
			}
			if hs, ok := decodeHookEvents(item, doc.path+"#hooks", ds); ok {
				out = append(out, hs)
			}
		}
	}
	return out
}

func appendHookFile(out []HookSource, file string, seen map[string]bool, ds *diagnostics) []HookSource {
	if seen[file] {
		return out
	}
	seen[file] = true
	raw, err := os.ReadFile(file)
	if err != nil {
		ds.errorf("hooks", file, RuleHooksShape, "read failed: %v", err)
		return out
	}
	if hs, ok := decodeHookEvents(raw, file, ds); ok {
		out = append(out, hs)
	}
	return out
}

// decodeHookEvents 接受 {"hooks": {Event: [...]}}（hooks.json，含可选 description）
// 或直接的 {Event: [...]}（Claude 清单内联，与 settings.json 同形）。
func decodeHookEvents(raw []byte, source string, ds *diagnostics) (HookSource, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		ds.errorf("hooks", source, RuleHooksShape, "hooks config must be a JSON object: %v", err)
		return HookSource{}, false
	}
	events := top
	if inner, ok := top["hooks"]; ok {
		events = nil
		if err := json.Unmarshal(inner, &events); err != nil {
			ds.errorf("hooks", source, RuleHooksShape, "\"hooks\" must map event names to matcher arrays")
			return HookSource{}, false
		}
	}
	delete(events, "description")
	for ev, groups := range events {
		var arr []json.RawMessage
		if json.Unmarshal(groups, &arr) != nil {
			ds.errorf("hooks", source, RuleHooksShape, "event %q must be an array of matcher groups", ev)
			return HookSource{}, false
		}
	}
	canon, _ := json.Marshal(events) // map 键有序编码，哈希与声明顺序无关
	sum := sha256.Sum256(canon)
	return HookSource{Source: source, Events: events, Digest: hex.EncodeToString(sum[:])}, true
}
