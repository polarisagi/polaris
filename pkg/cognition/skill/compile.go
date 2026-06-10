package skill

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// ValidateJS 校验 JS 代码安全性，禁用 eval/Function 等动态执行，及 require 等 Node 内置。
// 满足 Phase 3 AST 静态分析强制安全边界需求。
func ValidateJS(source string) error {
	prg, err := parser.ParseFile(nil, "", source, 0)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "js validation: parse error", err)
	}

	var validationErr error
	walkAST(prg, func(node ast.Node) bool {
		if validationErr != nil {
			return false
		}
		switch n := node.(type) {
		case *ast.Identifier:
			if n.Name == "eval" || n.Name == "Function" {
				validationErr = perrors.New(perrors.CodeInternal, fmt.Sprintf("js validation: dynamic execution is forbidden (found %s)", n.Name))
				return false
			}
		case *ast.CallExpression:
			if callee, ok := n.Callee.(*ast.Identifier); ok {
				if callee.Name == "require" || callee.Name == "process" {
					validationErr = perrors.New(perrors.CodeInternal, fmt.Sprintf("js validation: nodejs built-ins are forbidden (found %s)", callee.Name))
					return false
				}
			}
		}
		return true
	})

	return validationErr
}

// walkAST 利用反射递归遍历 goja AST
func walkAST(node ast.Node, visit func(ast.Node) bool) {
	if node == nil || !visit(node) {
		return
	}
	v := reflect.ValueOf(node)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		if field.Kind() == reflect.Slice {
			walkASTSlice(field, visit)
		} else if field.CanInterface() {
			if child, ok := field.Interface().(ast.Node); ok {
				walkAST(child, visit)
			}
		}
	}
}

// walkASTSlice 辅助处理 Slice 类型的 AST 节点遍历，降低嵌套复杂度
func walkASTSlice(field reflect.Value, visit func(ast.Node) bool) {
	for j := 0; j < field.Len(); j++ {
		elem := field.Index(j)
		if elem.CanInterface() {
			if child, ok := elem.Interface().(ast.Node); ok {
				walkAST(child, visit)
			}
		}
	}
}

// CompileJSToWasm 将验证通过的 JS 编译为 Wasm 字节码（底层调用 javy）。
// 此函数将原本的 wazero 编译路径切换到了 JS/TS 的 Component Model 路径（WASI）。
// 注意：运行环境需预装 javy CLI。
func CompileJSToWasm(ctx context.Context, jsCode string) ([]byte, error) {
	if err := ValidateJS(jsCode); err != nil {
		return nil, err
	}

	tmpDir, err := os.MkdirTemp("", "polaris-javy-*")
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "create temp dir", err)
	}
	defer os.RemoveAll(tmpDir)

	jsFile := filepath.Join(tmpDir, "index.js")
	wasmFile := filepath.Join(tmpDir, "index.wasm")

	if err := os.WriteFile(jsFile, []byte(jsCode), 0644); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "write js file", err)
	}

	cmd := exec.CommandContext(ctx, "javy", "compile", jsFile, "-o", wasmFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("javy compile failed: %s", string(out)), err)
	}

	wasmBytes, err := os.ReadFile(wasmFile)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "read wasm file", err)
	}

	return wasmBytes, nil
}
