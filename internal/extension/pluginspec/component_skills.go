package pluginspec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	RuleSkillDuplicate  = "skill.duplicate-name"
	RuleCommandShape    = "claude.commands.shape"
	RuleComponentPath   = "component.path"
	RuleSkillScanFailed = "skill.scan"
)

// loadSkills 技能发现：默认 skills/ + Claude/Codex 清单 skills 路径（追加语义），
// 无 skills/ 且无清单声明时，根目录 SKILL.md 按单技能插件加载（Claude 规则）。
func loadSkills(root, pluginName string, docs []manifestDoc, ds *diagnostics) []*Skill {
	var scanDirs []string
	if d, ok := existingDir(root, "skills"); ok {
		scanDirs = append(scanDirs, d)
	}
	declared := docsWithField(docs, func(w manifestWire) json.RawMessage { return w.Skills })
	for _, doc := range declared {
		paths, ok := rawPaths(doc.wire.Skills)
		if !ok {
			ds.errorf("skill", doc.path, RuleComponentPath, "skills must be a path or an array of paths")
			continue
		}
		for _, rel := range paths {
			abs, rule, err := resolveComponentPath(root, rel, true)
			if err != nil {
				ds.errorf("skill", doc.path, rule, "skills path %q: %v", rel, err)
				continue
			}
			scanDirs = append(scanDirs, abs)
		}
	}
	if len(scanDirs) == 0 && len(declared) == 0 {
		if _, ok := existingFile(root, "SKILL.md"); ok {
			return collectSkills([]string{root}, pluginName, true, ds)
		}
		return nil
	}
	return collectSkills(scanDirs, pluginName, false, ds)
}

// collectSkills 扫描目录：目录自身含 SKILL.md 即为单技能，否则取其直接子目录中的 SKILL.md。
// rootSingle 为 true 时根技能缺省名取插件名而非目录名。
func collectSkills(scanDirs []string, pluginName string, rootSingle bool, ds *diagnostics) []*Skill {
	seenFile := map[string]bool{}
	seenName := map[string]string{}
	var out []*Skill
	for _, dir := range scanDirs {
		for _, skillDir := range skillDirsIn(dir, ds) {
			if seenFile[skillDir] {
				continue
			}
			seenFile[skillDir] = true
			s, sd := parseSkillAt(skillDir, pluginName, rootSingle)
			*ds = append(*ds, sd...)
			if s == nil {
				continue
			}
			if prev, dup := seenName[s.Name]; dup {
				ds.errorf("skill", s.File, RuleSkillDuplicate, "skill name %q already declared by %s", s.Name, prev)
				continue
			}
			seenName[s.Name] = s.File
			out = append(out, s)
		}
	}
	return out
}

func parseSkillAt(skillDir, pluginName string, rootSingle bool) (*Skill, []Diagnostic) {
	if !rootSingle {
		return ParseSkillDir(skillDir)
	}
	var ds diagnostics
	s := parseSkillMarkdown(filepath.Join(skillDir, "SKILL.md"), skillDir, pluginName, SkillKindSkill, &ds)
	if s != nil {
		if cfg, ok := parseOpenAISkillYAML(skillDir, &ds); ok {
			s.OpenAI = cfg
		}
	}
	// 根技能目录名即插件目录名，"name 与目录名一致" 条款不适用。
	filtered := ds[:0]
	for _, d := range ds {
		if d.Rule != RuleSkillNameDir {
			filtered = append(filtered, d)
		}
	}
	return s, filtered
}

func skillDirsIn(dir string, ds *diagnostics) []string {
	if _, ok := existingFile(dir, "SKILL.md"); ok {
		return []string{dir}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		ds.errorf("skill", dir, RuleSkillScanFailed, "scan failed: %v", err)
		return nil
	}
	var out []string
	for _, e := range entries {
		// 隐藏目录不是技能（.git 等）；符号链接目录由 resolveComponentPath 以外的路径进入时不跟随。
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, ok := existingFile(dir, e.Name(), "SKILL.md"); ok {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// commandEntryWire Claude commands 对象映射的单项：source 与 content 必须恰好其一。
type commandEntryWire struct {
	Source       string   `json:"source"`
	Content      string   `json:"content"`
	Description  string   `json:"description"`
	ArgumentHint string   `json:"argumentHint"`
	Model        string   `json:"model"`
	AllowedTools []string `json:"allowedTools"`
}

// loadCommands Claude commands：清单声明替换默认 commands/ 目录。
func loadCommands(root string, docs []manifestDoc, ds *diagnostics) []*Skill {
	declared := docsWithField(docs, func(w manifestWire) json.RawMessage { return w.Commands })
	if len(declared) == 0 {
		if dir, ok := existingDir(root, "commands"); ok {
			return commandsFromDir(dir, dir, ds)
		}
		return nil
	}
	var out []*Skill
	for _, doc := range declared {
		out = append(out, commandsFromManifest(root, doc, ds)...)
	}
	return out
}

func commandsFromManifest(root string, doc manifestDoc, ds *diagnostics) []*Skill {
	if paths, ok := rawPaths(doc.wire.Commands); ok {
		var out []*Skill
		for _, rel := range paths {
			abs, rule, err := resolveComponentPath(root, rel, false)
			if err != nil {
				ds.errorf("command", doc.path, rule, "commands path %q: %v", rel, err)
				continue
			}
			if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
				out = append(out, commandsFromDir(abs, abs, ds)...)
			} else {
				out = appendCommandNamed(out, abs, strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs)), ds)
			}
		}
		return out
	}
	var entries map[string]commandEntryWire
	if err := json.Unmarshal(doc.wire.Commands, &entries); err != nil {
		ds.errorf("command", doc.path, RuleCommandShape, "commands must be a path, array of paths or object map")
		return nil
	}
	return commandsFromMap(root, doc.path, entries, ds)
}

func commandsFromMap(root, manifestPath string, entries map[string]commandEntryWire, ds *diagnostics) []*Skill {
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []*Skill
	for _, name := range names {
		e := entries[name]
		if (e.Source == "") == (e.Content == "") {
			ds.errorf("command", manifestPath, RuleCommandShape, "command %q must set exactly one of source or content", name)
			continue
		}
		var s *Skill
		if e.Source != "" {
			abs, rule, err := resolveComponentPath(root, e.Source, false)
			if err != nil {
				ds.errorf("command", manifestPath, rule, "command %q source %q: %v", name, e.Source, err)
				continue
			}
			s = parseSkillMarkdown(abs, filepath.Dir(abs), name, SkillKindCommand, ds)
		} else {
			s = &Skill{Kind: SkillKindCommand, Dir: root, File: manifestPath, Body: e.Content, UserInvocable: true}
		}
		if s == nil {
			continue
		}
		s.Name = name
		s.Description = firstNonEmpty(e.Description, s.Description)
		s.ArgumentHint = firstNonEmpty(e.ArgumentHint, s.ArgumentHint)
		s.Model = firstNonEmpty(e.Model, s.Model)
		if len(e.AllowedTools) > 0 {
			s.AllowedTools = e.AllowedTools
		}
		out = append(out, s)
	}
	return out
}

// commandsFromDir 平铺 .md 命令文件；子目录路径以 ":" 连接构成命令名（Claude 规则）。
func commandsFromDir(base, dir string, ds *diagnostics) []*Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		ds.errorf("command", dir, RuleSkillScanFailed, "scan failed: %v", err)
		return nil
	}
	var out []*Skill
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		switch {
		case strings.HasPrefix(e.Name(), "."):
			continue
		case e.IsDir():
			out = append(out, commandsFromDir(base, full, ds)...)
		case strings.EqualFold(filepath.Ext(e.Name()), ".md"):
			out = appendCommandNamed(out, full, commandName(base, full), ds)
		}
	}
	return out
}

func commandName(base, file string) string {
	rel, err := filepath.Rel(base, file)
	if err != nil {
		rel = filepath.Base(file)
	}
	rel = strings.TrimSuffix(filepath.ToSlash(rel), filepath.Ext(rel))
	return strings.ReplaceAll(rel, "/", ":")
}

// appendCommandNamed 命令名恒来自文件名；命令 frontmatter 不支持 name / paths（Claude 规则）。
func appendCommandNamed(out []*Skill, file, name string, ds *diagnostics) []*Skill {
	s := parseSkillMarkdown(file, filepath.Dir(file), name, SkillKindCommand, ds)
	if s == nil {
		return out
	}
	s.Name = name
	s.Paths = nil
	return append(out, s)
}
