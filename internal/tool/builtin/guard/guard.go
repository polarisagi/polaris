package guard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// 路径分类清单的单一事实源是 policy.PathCatalog（ADR-0121 决策四、ADR-0122 决策二）：
// 自保护路径任何情况拒绝；其余类别（凭据/持久化/系统/文件名）仅在调用没有授权凭据时拒绝
// （有凭据的调用已由 policy.Decide 判定过，命中目标会在那里被硬限制询问）。

func forbiddenErr(path string) error {
	return apperr.New(apperr.CodeForbidden, fmt.Sprintf("path_guard: path is in forbidden directory: %s", path))
}

// CheckSelfProtected 自保护路径（vault.key、config.toml、secrets/、data/…）在任何情况下拒绝。
func CheckSelfProtected(path string) error {
	if policy.ClassifyPath(path, false, nil).Self {
		return forbiddenErr(path)
	}
	return nil
}

// SelfProtectedMatcher 返回自保护路径匹配函数（目录只构造一次，供目录遍历逐项过滤）。
func SelfProtectedMatcher() func(path string) bool {
	c := policy.NewPathCatalog()
	return func(path string) bool { return c.Classify(path, false, nil).Self }
}

// CheckForbiddenPath 自保护恒拒 + 凭据/持久化/系统目录拒绝（无凭据语义，写口径；也是 CheckScopeRoot 的判据）。
// 不看文件名类（basename）：它只对可信根外的文件有意义，而本函数的入参是将成为可信根的目录。
func CheckForbiddenPath(path string) error {
	pc := policy.ClassifyPath(path, true, nil)
	if pc.Self || pc.Credential || pc.Persistence || pc.System {
		return forbiddenErr(path)
	}
	return nil
}

// forbiddenDirs 自保护 ∪ 凭据 ∪ 持久化 ∪ 系统目录（项目根的祖先判据用）。
func forbiddenDirs() []string {
	rw, wo := policy.SandboxDenyPaths()
	return append(rw, wo...)
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

// confinedRoot 受限文件根（AgentProfileSpec.FileRoot）。ok=true 时调用方只能用返回的根：
// 根本身不满足 CheckScopeRoot（落在敏感目录或是其祖先）时返回空集——CheckAllowedPath 对空集
// fail-closed，任何路径都不放行，而不是退回进程级白名单。
func confinedRoot(ctx context.Context) ([]string, bool) {
	root, ok := protocol.ConfinedRootFrom(ctx)
	if !ok {
		return nil, false
	}
	if CheckScopeRoot(root) != nil {
		return []string{}, true
	}
	return []string{filepath.Clean(root)}, true
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
	if confined, ok := confinedRoot(ctx); ok {
		return confined
	}
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
	if confined, ok := confinedRoot(ctx); ok {
		return confined
	}
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

// CheckReadablePath 读取类工具的统一路径门控（ADR-0121 决策三）：
//   - 自保护路径（原路径与软链真实路径）恒拒；
//   - 有授权凭据（Decide 已判定该调用）→ 放行，不再套更窄的白名单；
//   - 无凭据 → 可信根白名单 + 敏感路径拒绝 + 软链真实路径复核。
func CheckReadablePath(ctx context.Context, path string, allowedPaths []string) error {
	return checkPath(ctx, path, allowedPaths, false)
}

// CheckWritablePath 写入类工具的统一路径门控，语义同 CheckReadablePath，敏感清单取写口径
// （追加持久化落点与系统目录）。已存在路径（或其最近已存在祖先）按符号链接解析后的真实路径
// 再校验一次，防止白名单内的软链指向受保护目录。
func CheckWritablePath(ctx context.Context, path string, allowedPaths []string) error {
	return checkPath(ctx, path, allowedPaths, true)
}

func checkPath(ctx context.Context, path string, allowedPaths []string, write bool) error {
	if err := CheckSelfProtected(path); err != nil {
		return err
	}
	real, hasReal := resolveExistingPrefix(path)
	if hasReal {
		if err := CheckSelfProtected(real); err != nil {
			return err
		}
	}
	// 受限文件根（对端委派任务）替换而非追加白名单，授权凭据不得放宽它
	if _, confined := protocol.ConfinedRootFrom(ctx); !confined && protocol.PermissionGrantFrom(ctx) != protocol.GrantNone {
		return nil
	}
	if err := CheckAllowedPath(path, allowedPaths); err != nil {
		return err
	}
	// 目录内部同时按原路径与真实路径匹配；文件名类仅对白名单（可信根）之外的路径生效
	if policy.ClassifyPath(path, write, allowedPaths).Blocked() {
		return forbiddenErr(path)
	}
	if hasReal && real != filepath.Clean(path) {
		if err := CheckAllowedPath(real, resolveAll(allowedPaths)); err != nil {
			return apperr.Wrap(apperr.CodeForbidden, "path_guard: symlink escapes allowed paths", err)
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

// SessionIDFromCtx 取工具执行 ctx 中的会话 ID（protocol.CtxTaskIDKey，与
// llm_calls.session_id 同源）。为空返回 CodeInvalidInput——会话级状态工具
// 必须 fail-closed，禁止回落到共享存储把不同会话的数据混在一起。
func SessionIDFromCtx(ctx context.Context) (string, error) {
	sid, _ := ctx.Value(protocol.CtxTaskIDKey{}).(string)
	if sid == "" {
		return "", apperr.New(apperr.CodeInvalidInput, "缺少会话 ID（ctx 未携带 CtxTaskIDKey），拒绝访问会话级状态")
	}
	return sid, nil
}
