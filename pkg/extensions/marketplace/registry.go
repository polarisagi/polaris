package marketplace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"log/slog"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// Plugin is the runtime representation of a loaded plugin.
type Plugin struct {
	Manifest protocol.PluginJSON
	Dir      string
	Enabled  bool
	MCPs     map[string]protocol.MCPServerDef
}

// Registry 持有并管理已加载的 Plugin 实例。
// 线程安全；支持动态 enable/disable。
type Registry struct {
	mu      sync.RWMutex
	plugins map[string]*Plugin // name → Plugin
}

func NewRegistry() *Registry {
	return &Registry{plugins: make(map[string]*Plugin)}
}

// Register 注册一个 Plugin（已解析的 Manifest）。
// 同名重复注册返回错误（显式替换需先 Unregister）。
func (r *Registry) Register(p *Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.plugins[p.Manifest.Name]; exists {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("plugin: %q already registered; unregister first", p.Manifest.Name))
	}
	r.plugins[p.Manifest.Name] = p
	slog.Info("plugin: registered", "name", p.Manifest.Name)
	return nil
}

// Unregister 移除 Plugin（不卸载已注册的技能/MCP，由调用方负责）。
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.plugins, name)
	slog.Info("plugin: unregistered", "name", name)
}

// SetEnabled 启用/禁用 Plugin（不移除，只改 Enabled 标志）。
func (r *Registry) SetEnabled(name string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	p, ok := r.plugins[name]
	if !ok {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("plugin: %q not found", name))
	}
	p.Enabled = enabled
	slog.Info("plugin: status changed", "name", name, "enabled", enabled)
	return nil
}

// Get 返回 Plugin（不论 Enabled 状态）。
func (r *Registry) Get(name string) (*Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.plugins[name]
	return p, ok
}

// ListEnabled 返回所有 Enabled=true 的 Plugin 列表。
func (r *Registry) ListEnabled() []*Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out
}

// ScanDir 扫描目录（每个子目录作为一个 protocol.protocol.PluginJSONJSON），加载所有找到的 protocol.protocol.PluginJSONJSON。
// 忽略无 .polaris-plugin/plugin.json 的子目录。
func (r *Registry) ScanDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("plugin: scan %s: %v", dir, err), err)
	}

	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pluginDir := filepath.Join(dir, e.Name())
		p, err := LoadPlugin(pluginDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return count, err
		}
		if regErr := r.Register(p); regErr != nil {
			// 已注册则跳过，不报错
			continue
		}
		count++
	}
	return count, nil
}

// DefaultScanPaths 返回惯例扫描路径（内置级 + 项目级 + 用户级）。
// dataDir 为 DataLayout.Root（~/.polarisagi/polaris），用户安装路径为 dataDir/extensions。
func DefaultScanPaths(dataDir string) []string {
	cwd, _ := os.Getwd()
	return []string{
		filepath.Join(dataDir, "extensions"),
		filepath.Join(cwd, ".polaris", "plugins"),
	}
}
