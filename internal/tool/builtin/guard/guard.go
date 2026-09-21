package guard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// forbiddenDirs 敏感目录清单：写入类工具的黑名单，也是项目根（CheckScopeRoot）的判据。
// 同时收录各项的真实路径：macOS 上 /etc → /private/etc，调用方传入的往往是已
// EvalSymlinks 的规范路径（项目根、CheckWritablePath 的 real 分支），只比字面值会漏。
func forbiddenDirs() []string {
	base := forbiddenDirsLiteral()
	out := make([]string, 0, len(base)*2)
	for _, f := range base {
		out = append(out, f)
		if real, err := filepath.EvalSymlinks(f); err == nil && real != f {
			out = append(out, real)
		}
	}
	return out
}

func forbiddenDirsLiteral() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return []string{"/etc", "/usr", "/bin", "/sbin", "/root/.polarisagi"}
	}
	return []string{
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
}

func CheckForbiddenPath(path string) error {
	cleanPath := filepath.Clean(path)
	for _, f := range forbiddenDirs() {
		if cleanPath == f || strings.HasPrefix(cleanPath, f+string(filepath.Separator)) {
			return apperr.New(apperr.CodeForbidden, fmt.Sprintf("path_guard: path is in forbidden directory: %s", path))
		}
	}
	return nil
}

// CheckScopeRoot 校验一个目录能否作为会话级文件访问根（项目工作目录，ADR-0097 决策五）。
//
// 两条都要成立：
//  1. 不在敏感目录内（与 CheckForbiddenPath 同一清单）；
//  2. 不是用户主目录、也不是任一敏感目录的祖先——读类工具（read_file/grep/glob…）
//     只做白名单校验、不查黑名单，根若是 ~ 或 /Users，~/.ssh 就在可读范围内。
func CheckScopeRoot(root string) error {
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) {
		return apperr.New(apperr.CodeInvalidInput, "path_guard: scope root must be absolute")
	}
	if err := CheckForbiddenPath(clean); err != nil {
		return err
	}
	guarded := forbiddenDirs()
	if home, err := os.UserHomeDir(); err == nil {
		guarded = append(guarded, filepath.Clean(home))
		if real, err := filepath.EvalSymlinks(home); err == nil {
			guarded = append(guarded, real)
		}
	}
	// 根目录 "/" 的 Clean 结果已以分隔符结尾，不能再拼一个（否则前缀成了 "//"，永不命中）。
	prefix := clean
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	for _, g := range guarded {
		if g == clean || strings.HasPrefix(g, prefix) {
			return apperr.New(apperr.CodeForbidden,
				fmt.Sprintf("path_guard: %s 包含敏感目录或用户主目录（%s），不能作为项目工作目录", root, g))
		}
	}
	return nil
}

// scopeRoot 取 ctx 中的项目工作目录；不满足 CheckScopeRoot 时视为无（fail-closed：
// 写入侧已校验，此处是纵深防御——存库后规则收紧或目录被替换时不放大访问面）。
func scopeRoot(ctx context.Context) string {
	root := protocol.ProjectRootFrom(ctx)
	if root == "" || CheckScopeRoot(root) != nil {
		return ""
	}
	return filepath.Clean(root)
}

// ScopedPaths 本次调用的可访问根 = 项目工作目录（若有，置首位）+ 进程级白名单。
// 置首位是因为 bash/run_command 以 allowedPaths[0] 作默认工作目录：项目会话里
// 命令应在项目目录执行。
func ScopedPaths(ctx context.Context, base []string) []string {
	root := scopeRoot(ctx)
	if root == "" {
		return base
	}
	out := make([]string, 0, len(base)+1)
	out = append(out, root)
	for _, p := range base {
		if filepath.Clean(p) != root {
			out = append(out, p)
		}
	}
	return out
}

// SearchRoots grep/glob 未指定路径时的默认搜索根：项目会话只搜项目目录
// （把 DataDir 里的数据库/日志混进项目代码的搜索结果没有意义），否则沿用白名单。
func SearchRoots(ctx context.Context, base []string) []string {
	if root := scopeRoot(ctx); root != "" {
		return []string{root}
	}
	return base
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
