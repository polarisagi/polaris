package pluginspec

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Injection 技能正文中的动态上下文注入（Claude：`!`cmd“ 与 ```! 代码块）。
// 命令在技能内容交给模型之前执行，输出替换占位；执行与信任在运行时层处理。
type Injection struct {
	Start, End int // 占位在正文中的字节区间
	Command    string
}

// inlineInjection `!`cmd“ 仅在行首或空白之后识别（`KEY=!`cmd“ 不执行，Claude 规则）。
var inlineInjection = regexp.MustCompile("(^|[ \\t])!`([^`\\n]+)`")

// blockInjection ```! 开头、``` 结尾的多行命令块。
var blockInjection = regexp.MustCompile("(?m)^```!\\s*\\n((?s:.*?))\\n```[ \\t]*$")

// FindInjections 按出现顺序返回注入点；替换只做一遍，命令输出不再二次扫描。
func FindInjections(body string) []Injection {
	var out []Injection
	for _, m := range blockInjection.FindAllStringSubmatchIndex(body, -1) {
		out = append(out, Injection{Start: m[0], End: m[1], Command: strings.TrimSpace(body[m[2]:m[3]])})
	}
	for _, m := range inlineInjection.FindAllStringSubmatchIndex(body, -1) {
		start := m[4] - 2 // 指向 "!`"
		if overlaps(out, start) {
			continue
		}
		out = append(out, Injection{Start: start, End: m[1], Command: strings.TrimSpace(body[m[4]:m[5]])})
	}
	sortInjections(out)
	return out
}

func overlaps(list []Injection, pos int) bool {
	for _, in := range list {
		if pos >= in.Start && pos < in.End {
			return true
		}
	}
	return false
}

func sortInjections(list []Injection) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Start < list[j-1].Start; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// ReplaceInjections 以 outputs[i] 替换第 i 个注入占位。
func ReplaceInjections(body string, injections []Injection, outputs []string) string {
	var b strings.Builder
	last := 0
	for i, in := range injections {
		b.WriteString(body[last:in.Start])
		b.WriteString(outputs[i])
		last = in.End
	}
	b.WriteString(body[last:])
	return b.String()
}

// InjectionDigest 注入命令集合的哈希：信任绑定到这组命令，命令变更即回到待审。
func InjectionDigest(injections []Injection) string {
	h := sha256.New()
	for _, in := range injections {
		h.Write([]byte(in.Command))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
