package agents

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"regexp"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// CapabilitySet 表示沙箱能力集，用于权限判断
type CapabilitySet = map[string]bool

type codeRule struct {
	id          string
	description string
	requiredCap string
	pattern     *regexp.Regexp
}

// pythonDangerousPatterns Python 危险调用规则表。

// bashDangerousPatterns Bash 危险调用规则表。

func validateByPatterns(code []byte, rules []codeRule, caps CapabilitySet) error {
	for _, rule := range rules {
		if rule.pattern.Match(code) {
			if !caps[rule.requiredCap] {
				return apperr.New(apperr.CodeForbidden, fmt.Sprintf(
					"code validation failed: rule %s (%s) triggered, requires capability '%s'",
					rule.id, rule.description, rule.requiredCap))
			}
		}
	}
	return nil
}

// ValidateCode 扫描并校验生成代码的安全性（正则规则引擎）
func (ga *GovernanceAgent) ValidateCode(language string, code []byte, caps CapabilitySet) error {
	switch language {
	case "python":
		return validateByPatterns(code, ga.validatorRules.pythonDangerousPatterns, caps)
	case "bash", "sh":
		return validateByPatterns(code, ga.validatorRules.bashDangerousPatterns, caps)
	case "wasm":
		return ga.ValidateWasmImports(code, caps)
	case "typescript", "ts", "javascript", "js":
		return validateByPatterns(code, ga.validatorRules.typescriptDangerousPatterns, caps)
	default:
		// 未知语言：记录日志，不拦截（不能假设恶意）
		slog.Warn("code_validator: unknown language, skipping", "language", language)
		return nil
	}
}

// wasiAllowedImports 始终允许的 WASI 导入（无需能力）。

// wasiCapabilityGatedImports 需要特定能力才允许的 WASI 导入。

// readU32LEB128 手动解析 LEB128 的辅助函数
func readU32LEB128(data []byte, offset int) (uint32, int, error) {
	var result uint32
	var shift uint
	for i := offset; i < len(data); i++ {
		b := data[i]
		result |= uint32(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			return result, i + 1, nil
		}
	}
	return 0, offset, apperr.New(apperr.CodeInvalidInput, "LEB128 decoding error")
}

func skipKindData(kind byte, wasmBytes []byte, importOffset int) (int, error) {
	switch kind {
	case 0: // func
		_, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
		if err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "skipKindData", err)
		}
		return nextOffset, nil
	case 1: // table
		importOffset++ // ref_type
		limitsFlag := wasmBytes[importOffset]
		importOffset++
		_, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
		if err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "skipKindData", err)
		}
		if limitsFlag == 1 {
			_, nextOffset, err = readU32LEB128(wasmBytes, nextOffset)
			if err != nil {
				return nextOffset, apperr.Wrap(apperr.CodeInternal, "skipKindData", err)
			}
			return nextOffset, nil
		}
		return nextOffset, nil
	case 2: // mem
		limitsFlag := wasmBytes[importOffset]
		importOffset++
		_, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
		if err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "skipKindData", err)
		}
		if limitsFlag == 1 {
			_, nextOffset, err = readU32LEB128(wasmBytes, nextOffset)
			if err != nil {
				return nextOffset, apperr.Wrap(apperr.CodeInternal, "skipKindData", err)
			}
			return nextOffset, nil
		}
		return nextOffset, nil
	case 3: // global
		return importOffset + 2, nil
	}
	return importOffset, nil
}

func (ga *GovernanceAgent) validateImportSection(wasmBytes []byte, importOffset int, caps CapabilitySet) error {
	importCount, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GovernanceAgent.validateImportSection", err)
	}
	importOffset = nextOffset

	for i := uint32(0); i < importCount; i++ {
		modNameLen, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "GovernanceAgent.validateImportSection", err)
		}
		importOffset = nextOffset
		modName := string(wasmBytes[importOffset : importOffset+int(modNameLen)])
		importOffset += int(modNameLen)

		fieldNameLen, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "GovernanceAgent.validateImportSection", err)
		}
		importOffset = nextOffset
		fieldName := string(wasmBytes[importOffset : importOffset+int(fieldNameLen)])
		importOffset += int(fieldNameLen)

		kind := wasmBytes[importOffset]
		importOffset++

		importOffset, err = skipKindData(kind, wasmBytes, importOffset)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "GovernanceAgent.validateImportSection", err)
		}

		key := modName + ":" + fieldName

		if ga.validatorRules.wasiAllowedImports[key] {
			continue
		}

		if requiredCap, ok := ga.validatorRules.wasiCapabilityGatedImports[key]; ok {
			if caps != nil && caps[requiredCap] {
				continue
			}
			return apperr.New(apperr.CodeForbidden, fmt.Sprintf("wasm import %s requires capability %s", key, requiredCap))
		}

		return apperr.New(apperr.CodeForbidden, fmt.Sprintf("wasm import %s is not allowed by policy", key))
	}
	return nil
}

// ValidateWasmImports 解析 Wasm 二进制的 Import Section，
// 拒绝导入了白名单之外宿主函数的 Wasm 模块。
func (ga *GovernanceAgent) ValidateWasmImports(wasmBytes []byte, caps CapabilitySet) error {
	if len(wasmBytes) < 8 {
		return apperr.New(apperr.CodeInvalidInput, "invalid wasm file")
	}

	magic := []byte{0x00, 0x61, 0x73, 0x6d}
	version := []byte{0x01, 0x00, 0x00, 0x00}
	if !bytes.Equal(wasmBytes[0:4], magic) || !bytes.Equal(wasmBytes[4:8], version) {
		return apperr.New(apperr.CodeInvalidInput, "invalid wasm magic/version")
	}

	offset := 8
	for offset < len(wasmBytes) {
		sectionID := wasmBytes[offset]
		offset++

		sectionSize, nextOffset, err := readU32LEB128(wasmBytes, offset)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "GovernanceAgent.ValidateWasmImports", err)
		}
		offset = nextOffset

		if sectionID == 2 { // Import Section
			return ga.validateImportSection(wasmBytes, offset, caps)
		}

		offset += int(sectionSize)
	}

	return nil
}

// typescriptDangerousPatterns TypeScript/JavaScript 危险调用规则表。
// 设计说明：这是第一道静态防线（正则），覆盖"明显恶意"模式。
// 运行时第二道防线由 Deno 权限标志（capability flags）承担，见 plugin_creator.go。

// dangerousGoPackages 未持有对应 Capability 时禁止导入的包。

// auditGoAST 解析 Go 源码 AST，拦截未授权包导入。
// 仅扫描 import 声明，O(imports) 复杂度，不做全量 AST 遍历。
func (ga *GovernanceAgent) auditGoAST(code []byte, caps CapabilitySet) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", code, parser.ImportsOnly)
	if err != nil {
		// 解析失败 = 语法错误代码，阻断执行
		return apperr.New(apperr.CodeForbidden, "go AST parse failed: "+err.Error())
	}
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		// 去除引号
		pkg := strings.Trim(imp.Path.Value, `"`)
		if requiredCap, blocked := ga.validatorRules.dangerousGoPackages[pkg]; blocked {
			if !caps[requiredCap] {
				return apperr.New(apperr.CodeForbidden,
					fmt.Sprintf("AST: unauthorized import %q requires capability %q", pkg, requiredCap))
			}
		}
	}
	return nil
}

// pythonDangerousImports Python 禁止导入映射（import 行扫描，不用正则全文匹配）。

// bashDangerousCommands Bash 禁止命令映射（行首匹配）。

// tsDangerousImports TypeScript/JavaScript 禁止导入映射。

// auditImportLines 扫描代码每行，检测危险 import/require 语句。
// 不做完整 AST 解析，仅匹配 import/from/require 行，性能 O(lines)。
func auditImportLines(code []byte, dangerousMap map[string]string, caps CapabilitySet) error {
	for _, line := range strings.Split(string(code), "\n") {
		trimmed := strings.TrimSpace(line)
		// 跳过注释行
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		for keyword, requiredCap := range dangerousMap {
			if strings.Contains(trimmed, keyword) {
				if !caps[requiredCap] {
					return apperr.New(apperr.CodeForbidden,
						fmt.Sprintf("AST: dangerous import/use of %q requires capability %q", keyword, requiredCap))
				}
			}
		}
	}
	return nil
}
