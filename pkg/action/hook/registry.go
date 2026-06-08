package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"

	"gopkg.in/yaml.v3"
)

// Registry 加载并持有 Hook 配置。
// 从 ~/.polarisagi/polaris/hooks/hooks.yaml（用户级）和
// .polaris/hooks/hooks.yaml（项目级）合并加载。
// 高优先级（项目）不覆盖低优先级（用户）——两者均执行（与 Codex 语义一致）。
type Registry struct {
	groups map[Event][]MatcherGroup // event → 已编译的匹配组
}

// Load 加载 Hook 配置。paths 为 hooks.yaml 路径列表（低优先级在前）。
func Load(paths ...string) (*Registry, error) {
	merged := Config{Hooks: make(map[Event][]MatcherGroup)}

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("hook: read %s: %v", p, err), err)
		}

		var cfg Config
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("hook: parse %s: %v", p, err), err)
		}

		for event, groups := range cfg.Hooks {
			merged.Hooks[event] = append(merged.Hooks[event], groups...)
		}
	}

	r := &Registry{groups: make(map[Event][]MatcherGroup)}
	for event, groups := range merged.Hooks {
		r.groups[event] = compileMatchers(applyDefaults(groups))
	}
	return r, nil
}

// LoadDefault 按惯例路径加载（用户级 + 项目级）。
func LoadDefault() (*Registry, error) {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()

	paths := []string{
		filepath.Join(home, ".polarisagi/polaris", "hooks", "hooks.yaml"),
		filepath.Join(cwd, ".polaris", "hooks", "hooks.yaml"),
	}
	return Load(paths...)
}

// Match 返回匹配事件和工具名的所有 MatcherGroup。
// toolName 空字符串 = 匹配所有（用于 SessionStart / Stop 等无工具事件）。
func (r *Registry) Match(event Event, toolName string) []MatcherGroup {
	groups, ok := r.groups[event]
	if !ok {
		return nil
	}

	var matched []MatcherGroup
	for _, g := range groups {
		if matches(g, toolName) {
			matched = append(matched, g)
		}
	}
	return matched
}

func matches(g MatcherGroup, toolName string) bool {
	if g.Matcher == "" {
		return true
	}
	if g.compiled != nil {
		return g.compiled.MatchString(toolName)
	}
	return strings.Contains(toolName, g.Matcher)
}

func applyDefaults(groups []MatcherGroup) []MatcherGroup {
	for i := range groups {
		for j := range groups[i].Hooks {
			if groups[i].Hooks[j].Timeout <= 0 {
				groups[i].Hooks[j].Timeout = 30 * time.Second
			}
		}
	}
	return groups
}
