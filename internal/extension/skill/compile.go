package skill

import (
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// 2026-07-14（ADR-0051）：ValidateJS 删除——Skill 生成/编译管线当前只有 Python
// runtime 真正被使用（generate.go 唯一调用 ValidatePython），全仓没有对应的
// TypeScript/JS skill 生成路径消费本函数，是为未支持的 runtime 预写的孤立校验器。
// 若未来真的支持 TS/JS skill runtime，应随该 runtime 的生成/编译管线一并设计，
// 而非现在保留一个没有配套生成逻辑的校验函数。

// ValidatePython 对 LLM 生成的 Python 代码做静态安全检查。
func ValidatePython(code string) error {
	if strings.Contains(code, "import os") || strings.Contains(code, "import subprocess") || strings.Contains(code, "import socket") || strings.Contains(code, "eval(") || strings.Contains(code, "exec(") {
		return apperr.New(apperr.CodeInternal, "dynamic execution and unsafe modules are forbidden in python: os, subprocess, socket, eval, exec")
	}
	return nil
}
