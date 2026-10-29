package pluginspec

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// 路径规则（Claude / Codex / agent-plugins 三方一致）：清单里的组件路径必须以 "./" 开头，
// 解析后必须留在插件根内且存在。"." 仅 Claude `skills` 字段允许（表示插件根）。
const (
	RulePathPrefix      = "path.dot-slash-prefix"
	RulePathContainment = "path.containment"
	RulePathExists      = "path.exists"
)

var errPathRule = apperr.New(apperr.CodeInvalidInput, "pluginspec: component path violates path rules")

// resolveComponentPath 把清单里的相对路径解析为插件根内的绝对路径。
// allowRootDot 为 true 时接受 "." 作为插件根（Claude skills 字段）。
// 返回的 rule 在失败时标识被违反的条款。
func resolveComponentPath(root, rel string, allowRootDot bool) (abs, rule string, err error) {
	switch {
	case rel == "." && allowRootDot:
		rel = "./"
	case !strings.HasPrefix(rel, "./"):
		return "", RulePathPrefix, errPathRule
	}
	// 反斜杠在 macOS/Linux 上被 Claude 拒绝；统一按不可移植路径处理。
	if strings.Contains(rel, `\`) || containsDotDot(rel) {
		return "", RulePathContainment, errPathRule
	}
	abs = filepath.Join(root, filepath.FromSlash(rel))
	if !withinRoot(root, abs) {
		return "", RulePathContainment, errPathRule
	}
	if _, statErr := os.Stat(abs); statErr != nil {
		return "", RulePathExists, errPathRule
	}
	// 符号链接可把包内路径指向包外（例如 ./skills -> /etc），按解析后的真实路径再判一次。
	real, evalErr := filepath.EvalSymlinks(abs)
	if evalErr != nil {
		return "", RulePathExists, errPathRule
	}
	realRoot, rootErr := filepath.EvalSymlinks(root)
	if rootErr != nil || !withinRoot(realRoot, real) {
		return "", RulePathContainment, errPathRule
	}
	return abs, "", nil
}

func containsDotDot(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func withinRoot(root, p string) bool {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return r == "." || (r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)))
}

// existingFile 判断插件根下默认位置文件是否存在（默认位置不经清单声明，不做 "./" 前缀校验）。
func existingFile(root string, parts ...string) (string, bool) {
	p := filepath.Join(append([]string{root}, parts...)...)
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return "", false
	}
	return p, true
}

func existingDir(root string, parts ...string) (string, bool) {
	p := filepath.Join(append([]string{root}, parts...)...)
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return p, true
}
