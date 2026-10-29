package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync/atomic"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Scope 来源作用域。
type Scope string

const (
	ScopeUser    Scope = "user"    // 本机用户级 hooks.json（管理员配置，视为已信任）
	ScopeProject Scope = "project" // 项目目录 .polaris/hooks/hooks.json（可随仓库带入，须审阅信任）
	ScopePlugin  Scope = "plugin"  // 已安装插件（须审阅信任）
)

// Source 一份 hook 配置来源及其执行上下文。
type Source struct {
	Key        string // 信任键：scope:定位符（插件为 plugin:<id>:<文件>）
	Scope      Scope
	PluginID   string
	PluginName string
	PluginRoot string            // ${PLUGIN_ROOT} / ${CLAUDE_PLUGIN_ROOT}
	PluginData string            // ${PLUGIN_DATA} / ${CLAUDE_PLUGIN_DATA}
	Options    map[string]string // userConfig 取值：CLAUDE_PLUGIN_OPTION_<KEY> 与 exec 形式 ${user_config.*}
	Digest     string            // 定义内容哈希；信任绑定到哈希，定义变更即回到待审
	Trusted    bool
	Config     Config
}

// SourceProvider 列出当前生效的全部来源（含信任状态）。consumer-side 定义：
// 插件来源由 extension/lifecycle 从 plugins.manifest 与信任存储构建。
type SourceProvider interface {
	ListHookSources(ctx context.Context) ([]Source, error)
}

// Registry 来源快照；Reload 原子替换，Dispatch 路径无锁读取。
type Registry struct {
	provider atomic.Pointer[SourceProvider]
	snapshot atomic.Pointer[[]Source]
}

func NewRegistry(provider SourceProvider) *Registry {
	r := &Registry{}
	if provider != nil {
		r.provider.Store(&provider)
	}
	empty := []Source{}
	r.snapshot.Store(&empty)
	return r
}

// SetProvider 启动期注入来源提供者（其依赖的仓库与配置服务晚于执行信封构造）。
func (r *Registry) SetProvider(p SourceProvider) {
	r.provider.Store(&p)
}

// Reload 重建快照（插件安装/卸载/启停、信任变更、hooks.json 修改后调用）。
func (r *Registry) Reload(ctx context.Context) error {
	pp := r.provider.Load()
	if pp == nil || *pp == nil {
		return nil
	}
	sources, err := (*pp).ListHookSources(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "hook.Registry.Reload", err)
	}
	r.snapshot.Store(&sources)
	return nil
}

// Sources 当前快照（只读）。
func (r *Registry) Sources() []Source {
	return *r.snapshot.Load()
}

// boundHandler 命中的处理器及其来源上下文。
type boundHandler struct {
	Handler Handler
	Source  *Source
}

// match 返回已信任来源中匹配 event + subject 的处理器；未信任来源一律跳过。
func (r *Registry) match(event Event, subject string) []boundHandler {
	sources := r.Sources()
	var out []boundHandler
	for i := range sources {
		src := &sources[i]
		if !src.Trusted {
			continue
		}
		for gi := range src.Config[event] {
			g := &src.Config[event][gi]
			if !g.Matches(subject) {
				continue
			}
			for _, h := range g.Hooks {
				out = append(out, boundHandler{Handler: h, Source: src})
			}
		}
	}
	return out
}

// Digest 事件定义的规范化哈希（与 pluginspec.HookSource.Digest 同算法：按键排序的 JSON）。
func Digest(events map[string]json.RawMessage) string {
	canon, err := json.Marshal(events)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// FileSource 读取用户级 / 项目级 hooks.json；文件不存在返回 ok=false。
func FileSource(scope Scope, path string) (Source, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Source{}, false, nil
		}
		return Source{}, false, apperr.Wrap(apperr.CodeInternal, "hook: read "+path, err)
	}
	var top struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return Source{}, false, apperr.Wrap(apperr.CodeInvalidInput, "hook: parse "+path, err)
	}
	cfg, err := ParseEvents(top.Hooks)
	if err != nil {
		return Source{}, false, err
	}
	return Source{Key: string(scope) + ":" + path, Scope: scope, Digest: Digest(top.Hooks), Config: cfg,
		Trusted: scope == ScopeUser}, true, nil
}
