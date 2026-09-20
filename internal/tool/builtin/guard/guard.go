package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func CheckForbiddenPath(path string) error {
	cleanPath := filepath.Clean(path)
	home, err := os.UserHomeDir()
	var forbidden []string
	if err == nil {
		forbidden = []string{
			filepath.Join(home, ".polarisagi", "polaris", "config"),
			filepath.Join(home, ".polarisagi", "polaris", "data"),
			filepath.Join(home, ".polarisagi", "polaris", "secrets"),
			filepath.Join(home, ".polarisagi", "polaris", "audit"),
			filepath.Join(home, ".ssh"),
			filepath.Join(home, ".gnupg"),
			"/etc",
			"/usr",
			"/bin",
			"/sbin",
		}
	} else {
		forbidden = []string{"/etc", "/usr", "/bin", "/sbin", "/root/.polarisagi"}
	}

	for _, f := range forbidden {
		if cleanPath == f || strings.HasPrefix(cleanPath, f+string(filepath.Separator)) {
			return apperr.New(apperr.CodeForbidden, fmt.Sprintf("path_guard: path is in forbidden directory: %s", path))
		}
	}
	return nil
}

func CheckAllowedPath(path string, allowedPaths []string) error {
	if len(allowedPaths) == 0 {
		return apperr.New(apperr.CodeInternal, "path_guard: no allowed paths configured (fail-closed)")
	}
	clean := filepath.Clean(path)
	for _, allowed := range allowedPaths {
		allowedClean := filepath.Clean(allowed)
		if clean == allowedClean || strings.HasPrefix(clean, allowedClean+string(filepath.Separator)) {
			return nil
		}
	}
	return apperr.New(apperr.CodeForbidden, fmt.Sprintf("path_guard: path %q not in allowed paths", path))

}

// CheckWritablePath 写入类工具的统一路径门控：白名单归属 + 敏感目录黑名单，二者缺一不可
// （GR-5.2-003：此前仅 write_file 同时校验，str_replace_editor/multi_edit/notebook_edit
// 只查白名单，allowedPaths 覆盖 $HOME 时可直接改写 ~/.ssh/authorized_keys 等）。
// 已存在路径（或其最近已存在祖先）按符号链接解析后的真实路径再校验一次，
// 防止白名单内的软链指向敏感目录。
func CheckWritablePath(path string, allowedPaths []string) error {
	if err := CheckAllowedPath(path, allowedPaths); err != nil {
		return err
	}
	if err := CheckForbiddenPath(path); err != nil {
		return err
	}
	if real, ok := resolveExistingPrefix(path); ok && real != filepath.Clean(path) {
		if err := CheckAllowedPath(real, resolveAll(allowedPaths)); err != nil {
			return apperr.Wrap(apperr.CodeForbidden, "path_guard: symlink escapes allowed paths", err)
		}
		if err := CheckForbiddenPath(real); err != nil {
			return err
		}
	}
	return nil
}

// resolveExistingPrefix 对路径中已存在的最长前缀做 EvalSymlinks，再拼回尚不存在的尾部。
func resolveExistingPrefix(path string) (string, bool) {
	clean := filepath.Clean(path)
	rest := ""
	cur := clean
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest), true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func resolveAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if real, ok := resolveExistingPrefix(p); ok {
			out = append(out, real)
		} else {
			out = append(out, p)
		}
	}
	return out
}

func IsPathAllowed(path string, allowedPaths []string) bool {
	if len(allowedPaths) == 0 {
		return false // fail-closed：空白名单拒绝所有
	}
	cleanPath := filepath.Clean(path)
	for _, allowed := range allowedPaths {
		cleanAllowed := filepath.Clean(allowed)
		// 精确匹配或严格子路径匹配（必须紧跟分隔符，防止前缀混淆）
		if cleanPath == cleanAllowed ||
			strings.HasPrefix(cleanPath, cleanAllowed+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func GetTodoPath(allowedPaths []string) (string, error) {
	if len(allowedPaths) == 0 {
		return "", apperr.New(apperr.CodeInternal, "todo: no workspace configured")
	}
	return filepath.Join(allowedPaths[0], ".polaris_todo.json"), nil
}
