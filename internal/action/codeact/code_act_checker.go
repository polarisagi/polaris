package codeact

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-python/gpython/ast"
	"github.com/go-python/gpython/parser"
	"github.com/go-python/gpython/py"

	"mvdan.cc/sh/v3/syntax"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// LLMPeerReviewer (L2) 检查代码意图。
// nil 表示跳过检查。
type LLMPeerReviewer interface {
	Review(ctx context.Context, code string) (risk string, err error)
}

// ASTChecker (L0) 静态语言级检查器
// nil 表示跳过检查。
type ASTChecker interface {
	CheckPython(code []byte) error
	CheckBash(code []byte) error
}

// DefaultASTChecker 实现 L0 静态检查。
type DefaultASTChecker struct{}

// CheckPython 阻止 os.system/exec、subprocess 族、eval/exec/__import__ 等危险调用。
// 采用字符串模式预检 + AST 精确检查双层策略：
//   - 字符串层：拦截 ctypes/cffi/importlib/__import__ 等 AST 难以静态追踪的危险导入
//   - AST 层：遍历调用节点，追踪模块别名（import os as o），精确拦截方法调用
func (d *DefaultASTChecker) CheckPython(code []byte) error {
	// L0-A：字符串模式检查（ctypes/cffi/importlib 等 FFI/动态导入绕过路径）
	if err := d.checkPythonStringPatterns(code); err != nil {
		return fmt.Errorf("l0_ast.CheckPython: %w", err)
	}

	mod, err := parser.ParseString(string(code), py.ExecMode)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "l0_ast: parse python failed", err)
	}

	// 追踪危险模块别名（防 import os as o; o.system() 绕过）
	// 单次 Walk 同时收集别名和检查调用，按 Python 语义顺序（别名可在调用后声明，
	// 但恶意代码通常先导入后使用；保守策略：任意顺序均检查）。
	dangerousAliases := make(map[string]string) // alias → original module

	var visitErr error
	ast.Walk(mod, func(node ast.Ast) bool {
		if visitErr != nil || node == nil {
			return visitErr == nil && node == nil
		}
		switch n := node.(type) {
		case *ast.Import:
			// 收集危险模块别名（import os as o → dangerousAliases["o"]="os"）
			for _, alias := range n.Names {
				modName := string(alias.Name)
				switch modName {
				case "os", "subprocess", "ctypes", "cffi":
					asName := string(alias.AsName)
					if asName == "" {
						asName = modName
					}
					dangerousAliases[asName] = modName
				}
			}
		case *ast.Call:
			visitErr = d.checkPythonCall(n, dangerousAliases)
			if visitErr != nil {
				return false
			}
		}
		return true
	})
	return visitErr
}

// checkPythonStringPatterns 字符串模式预检，覆盖 AST 静态分析盲区。
// 采用保守策略（误报多于漏报）：含危险模块名即拦截，不区分注释/字符串。
func (d *DefaultASTChecker) checkPythonStringPatterns(code []byte) error {
	s := string(code)
	// ctypes/cffi：FFI 直接调用 C 函数，可绕过 Python 安全沙箱
	if strings.Contains(s, "ctypes") {
		return apperr.New(apperr.CodeForbidden, "l0_ast: python ctypes usage not permitted (FFI bypass)")
	}
	if strings.Contains(s, "cffi") {
		return apperr.New(apperr.CodeForbidden, "l0_ast: python cffi usage not permitted (FFI bypass)")
	}
	// importlib：动态导入，可规避 import 级别的静态拦截
	if strings.Contains(s, "importlib") {
		return apperr.New(apperr.CodeForbidden, "l0_ast: python importlib not permitted (dynamic import bypass)")
	}
	// __import__：内置动态导入函数（AST 层亦检查，此处作字符串二重校验）
	if strings.Contains(s, "__import__") {
		return apperr.New(apperr.CodeForbidden, "l0_ast: python __import__() not permitted")
	}
	return nil
}

// checkPythonCall 检查单个 Call 节点，使用 dangerousAliases 追踪模块别名。
func (d *DefaultASTChecker) checkPythonCall(n *ast.Call, dangerousAliases map[string]string) error {
	// 内置危险函数
	if name, ok := n.Func.(*ast.Name); ok {
		id := string(name.Id)
		switch id {
		case "eval", "exec", "__import__":
			return apperr.New(apperr.CodeForbidden, "l0_ast: python dangerous builtin call: "+id)
		}
		return nil
	}

	attr, ok := n.Func.(*ast.Attribute)
	if !ok {
		return nil
	}

	attrName := string(attr.Attr)
	val, ok2 := attr.Value.(*ast.Name)
	if !ok2 {
		return nil
	}

	pkg := string(val.Id)
	// 解析别名 → 原始模块名
	origPkg := pkg
	if orig, aliased := dangerousAliases[pkg]; aliased {
		origPkg = orig
	}

	switch origPkg {
	case "os":
		switch attrName {
		case "system", "popen", "execv", "execve", "execvp", "execvpe",
			"spawnl", "spawnle", "spawnlp", "spawnlpe":
			return apperr.New(apperr.CodeForbidden, "l0_ast: python dangerous os call: "+pkg+"."+attrName)
		}
	case "subprocess":
		switch attrName {
		case "Popen", "run", "call", "check_output", "check_call":
			return apperr.New(apperr.CodeForbidden, "l0_ast: python dangerous subprocess call: "+pkg+"."+attrName)
		}
	case "ctypes", "cffi":
		return apperr.New(apperr.CodeForbidden, "l0_ast: python dangerous FFI call: "+pkg+"."+attrName)
	}

	return nil
}

// CheckBash 阻止 exec/eval/source/rm -rf 等危险命令，以及 /dev/tcp 网络绕过。
// 采用字符串模式预检 + AST 精确检查双层策略。
func (d *DefaultASTChecker) CheckBash(code []byte) error {
	// L0-A：字符串模式检查（/dev/tcp 网络绕过、变量间接引用）
	if err := d.checkBashStringPatterns(string(code)); err != nil {
		return fmt.Errorf("l0_ast.CheckBash: %w", err)
	}

	f, err := syntax.NewParser().Parse(strings.NewReader(string(code)), "")
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "l0_ast: parse bash failed", err)
	}

	var visitErr error
	syntax.Walk(f, func(node syntax.Node) bool {
		if visitErr != nil || node == nil {
			return visitErr == nil && node == nil
		}

		cmd, ok := node.(*syntax.CallExpr)
		if !ok || len(cmd.Args) == 0 {
			return true
		}

		name := cmd.Args[0].Parts[0]
		word, ok2 := name.(*syntax.Lit)
		if !ok2 {
			return true
		}

		switch word.Value {
		case "eval", "exec":
			visitErr = apperr.New(apperr.CodeForbidden, "l0_ast: bash dangerous builtin: "+word.Value)
			return false
		case "source", ".":
			// source/dot 可包含并执行任意脚本，等价于代码注入
			visitErr = apperr.New(apperr.CodeForbidden, "l0_ast: bash source/dot command not permitted")
			return false
		case "rm":
			if len(cmd.Args) > 1 {
				visitErr = d.checkBashRM(cmd)
				if visitErr != nil {
					return false
				}
			}
		}

		return true
	})
	return visitErr
}

// checkBashStringPatterns 字符串模式预检，拦截 AST 难以静态检测的 bash 危险用法。
func (d *DefaultASTChecker) checkBashStringPatterns(code string) error {
	// /dev/tcp 和 /dev/udp：bash 特殊伪文件，可建立任意 TCP/UDP 连接绕过网络限制
	if strings.Contains(code, "/dev/tcp") || strings.Contains(code, "/dev/udp") {
		return apperr.New(apperr.CodeForbidden,
			"l0_ast: bash /dev/tcp or /dev/udp network bypass not permitted")
	}
	// ${!var}：变量间接引用，可动态拼接并执行任意命令名
	if strings.Contains(code, "${!") {
		return apperr.New(apperr.CodeForbidden,
			"l0_ast: bash variable indirection ${!var} not permitted")
	}
	return nil
}

func (d *DefaultASTChecker) checkBashRM(cmd *syntax.CallExpr) error {
	for _, arg := range cmd.Args[1:] {
		if len(arg.Parts) > 0 {
			if word, ok := arg.Parts[0].(*syntax.Lit); ok {
				if strings.Contains(word.Value, "-r") || strings.Contains(word.Value, "-f") {
					return apperr.New(apperr.CodeForbidden, "l0_ast: bash dangerous command: rm -rf")
				}
			}
		}
	}
	return nil
}
