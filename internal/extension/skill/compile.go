package skill

import (
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ValidateJS 对 LLM 生成的 TypeScript/JavaScript 代码做静态安全检查。
// 禁止动态执行（eval/new Function）、Node.js 内置模块直接 require。
func ValidateJS(code string) error {
	if strings.Contains(code, "eval(") || strings.Contains(code, "new Function(") {
		return apperr.New(apperr.CodeInternal, "dynamic execution is forbidden: eval / new Function")
	}
	// 禁止裸 require（TypeScript 技能走 import，不走 CommonJS require）
	if strings.Contains(code, "require(") {
		return apperr.New(apperr.CodeInternal, "nodejs built-ins are forbidden: use import instead of require")
	}
	return nil
}
