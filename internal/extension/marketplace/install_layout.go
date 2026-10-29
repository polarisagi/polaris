package marketplace

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/util"
)

// 安装身份与目录（ADR-0124）：实例 ID 与安装路径都由 identity 确定性派生，卸载重装、升级后不变——
// 插件写进浏览器扩展目录、Native Messaging 宿主脚本等进程外配置的路径因此长期有效。

const (
	installedSubdir = "installed"
	sourcePrefix    = "_source/"
	localPrefix     = "_local/"
)

var unsafePathChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// CatalogIdentity 市场安装的身份即 catalog_id（`<marketplace_id>/<name>`）。
func CatalogIdentity(catalogID string) string { return catalogID }

// SourceIdentity 直接来源安装（不属于任何市场）的身份。
func SourceIdentity(name string) string { return sourcePrefix + name }

// LocalIdentity PluginCreator 意图生成插件的身份。
func LocalIdentity(name string) string { return localPrefix + name }

// InstanceID extension_instances.id；name 只为可读，唯一性由 identity 的哈希承担。
func InstanceID(name, identity string) string {
	return util.DeterministicID("ext", name, identity)
}

// InstallDir identity 对应的安装目录（root 为 extensions 根）。identity 来自市场数据，逐段清洗并拒绝
// 空段与 . / ..，保证结果落在 root/installed 之内。
func InstallDir(root, identity string) (string, error) {
	base := filepath.Join(root, installedSubdir)
	parts := []string{base}
	for _, seg := range strings.Split(identity, "/") {
		seg = unsafePathChars.ReplaceAllString(seg, "_")
		if seg == "" || seg == "." || seg == ".." {
			return "", apperr.New(apperr.CodeInvalidInput, "marketplace: invalid install identity "+identity)
		}
		parts = append(parts, seg)
	}
	dir := filepath.Join(parts...)
	if rel, err := filepath.Rel(base, dir); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", apperr.New(apperr.CodeInvalidInput, "marketplace: install identity escapes root "+identity)
	}
	return dir, nil
}
