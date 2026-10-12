package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// KeyPatch 对用户 config.toml 中一个键的改写：Section 为表名（如 "mesh"，不支持数组表 [[…]]），
// Value 为字符串 / 布尔 / 整数 / 字符串切片。
type KeyPatch struct {
	Section string
	Key     string
	Value   any
}

var keyNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// PatchUserConfig 就地改写用户 config.toml 的若干键（设置页使用），只动目标行：注释、空行、
// 其余键的书写顺序原样保留，行尾注释也保留。键所在的表不存在时追加到文件末尾，键不存在时
// 追加到该表最后一个非空行之后。
//
// 写入前用 Load 完整解析 + Validate 一遍改写结果（临时文件），不通过则原文件不动；
// 通过后原子替换（同目录临时文件 + rename），文件权限保持 0600。
// 配置文件不存在时以内嵌 defaults.toml 为底稿（与 Load 首次启动导出的内容一致）。
func PatchUserConfig(path string, patches []KeyPatch) error {
	if path == "" {
		return apperr.New(apperr.CodeInvalidInput, "config: no config file path")
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return apperr.Wrap(apperr.CodeInternal, "config: read "+path, err)
		}
		if orig, err = configs.FS.ReadFile("defaults.toml"); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "config: read embedded defaults", err)
		}
	}
	text := string(orig)
	for _, p := range patches {
		if !keyNameRe.MatchString(p.Section) || !keyNameRe.MatchString(p.Key) {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("config: invalid key %s.%s", p.Section, p.Key))
		}
		lit, err := tomlLiteral(p.Value)
		if err != nil {
			return err
		}
		text = patchKey(text, p.Section, p.Key, lit)
	}

	return writeValidated(path, text)
}

// writeValidated 把 text 写到 path 同目录的临时文件（0600），用 Load 完整解析 + Validate，
// 通过后原子替换 path；任何一步失败 path 保持原样。
func writeValidated(path, text string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "config: create config dir", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "config: create temp file", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // rename 成功后临时文件已不存在；失败路径兜底清理
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close()
		return apperr.Wrap(apperr.CodeInternal, "config: write temp file", err)
	}
	if err := tmp.Close(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "config: close temp file", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "config: chmod temp file", err)
	}
	if _, err := Load(tmpName); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "config: patched config does not validate, file left unchanged", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "config: replace "+path, err)
	}
	return nil
}

// tomlLiteral 把值编码成 TOML 右值（借 go-toml 编码单键表再取等号右侧，保证转义正确）。
func tomlLiteral(v any) (string, error) {
	switch v.(type) {
	case string, bool, int, int64, []string:
	default:
		return "", apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("config: unsupported value type %T", v))
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.SetArraysMultiline(false)
	if err := enc.Encode(map[string]any{"v": v}); err != nil {
		return "", apperr.Wrap(apperr.CodeInvalidInput, "config: encode value", err)
	}
	_, rhs, ok := strings.Cut(strings.TrimSpace(buf.String()), "=")
	if !ok {
		return "", apperr.New(apperr.CodeInternal, "config: unexpected encoder output")
	}
	return strings.TrimSpace(rhs), nil
}

// patchKey 在 text 中把 [section] 下的 key 改为 lit（逐行扫描，不重排）。
func patchKey(text, section, key, lit string) string {
	lines := strings.Split(text, "\n")
	start, end := sectionBounds(lines, section)
	newLine := key + " = " + lit
	if start < 0 {
		trimmed := strings.TrimRight(text, "\n")
		return trimmed + "\n\n[" + section + "]\n" + newLine + "\n"
	}
	keyRe := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	lastContent := start
	for i := start + 1; i < end; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			lastContent = i
		}
		if keyRe.MatchString(lines[i]) {
			indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
			if c := trailingComment(lines[i]); c != "" {
				lines[i] = indent + newLine + "  " + c
			} else {
				lines[i] = indent + newLine
			}
			return strings.Join(lines, "\n")
		}
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:lastContent+1]...)
	out = append(out, newLine)
	out = append(out, lines[lastContent+1:]...)
	return strings.Join(out, "\n")
}

// sectionBounds 返回 [section] 头所在行与下一个表头（或文件末尾）的行号；不存在时 start=-1。
func sectionBounds(lines []string, section string) (int, int) {
	start := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "[") {
			continue
		}
		if start >= 0 {
			return start, i
		}
		if strings.HasPrefix(t, "[[") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(t, "["), "]")
		if strings.TrimSpace(name) == section {
			start = i
		}
	}
	return start, len(lines)
}

// trailingComment 取一行赋值语句的行尾注释（"# …"），字符串里的 # 不算。多行字符串不在支持范围
// （设置页只写单行标量与单行数组）。
func trailingComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '"' && c == '\\':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return strings.TrimSpace(line[i:])
		}
	}
	return ""
}
