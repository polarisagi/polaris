//go:build ignore

// native_dialog_check 禁止 web/src 直接调用浏览器原生对话框 confirm()/alert()/prompt()（L-22）。
//
// 背景：桌面版是 Tauri v2 的 WKWebView，未装 dialog 插件时 window.confirm 不弹框、直接返回
// false——「删除项目」之类操作静默无反应（2026-10-20）。所有确认/提示一律走应用内
// Alpine.store('dialog')（web/src/js/store/dialog.js），文本经 x-text 渲染，浏览器与桌面表现一致。
//
// 判据：剥掉注释与字符串字面量后，出现「裸调用」或 window. 前缀的
// confirm( / alert( / prompt( 即违规。形如 $store.dialog.confirm( 的方法调用不是原生对话框，
// 放行；dialog.js 自身（定义处）整文件豁免。
//
// 2026-10-20 新增，fail-closed（存量为 0）。
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/polarisagi/polaris/tools/lintutil"
)

const (
	scanRoot   = "web/src"
	selfDialog = "web/src/js/store/dialog.js"
)

// 前一个字符不是标识符字符或 '.'（排除 x.confirm( / myconfirm(），或显式 window. 前缀。
var nativeCall = regexp.MustCompile(`(?:^|[^\w.$])(?:window\.)?(confirm|alert|prompt)\s*\(`)
var windowCall = regexp.MustCompile(`\bwindow\.(confirm|alert|prompt)\s*\(`)

func main() {
	r := lintutil.NewReporter("native-dialog-check", nil)

	if _, err := os.Stat(scanRoot); err != nil {
		r.Fatalf("扫描根 %s 不存在——规则的扫描面已失效", scanRoot)
	}
	filepath.Walk(scanRoot, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err != nil || info.IsDir() {
			return nil
		}
		slash := filepath.ToSlash(path)
		if slash == selfDialog {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".js" && ext != ".html" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			r.Fatalf("读取 %s 失败: %v", path, rerr)
		}
		r.Anchor() // 每个被扫描的前端文件都是判据面
		for i, line := range stripComments(string(data)) {
			if nativeCall.MatchString(line) || windowCall.MatchString(line) {
				r.Violation(slash+":"+itoa(i+1), "直接调用原生 confirm/alert/prompt；桌面 WKWebView 下不弹框，"+
					"请改用 Alpine.store('dialog').confirm/alert（违反 L-22）")
			}
		}
		return nil
	})

	r.RequireAnchors(10, "判据面是 web/src 下的 .js/.html 文件；若扫描根或目录结构变了请同步 scanRoot")
	r.Done()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// stripComments 返回逐行文本，其中块注释（/* */、<!-- -->）、行注释（//）与
// 字符串字面量内容被替换为空；行数与原文一致，便于定位。
// 不是完整的 JS 词法分析：模板字符串里的 ${...} 表达式按字符串处理（宁可漏报不误报）。
func stripComments(src string) []string {
	lines := strings.Split(src, "\n")
	out := make([]string, len(lines))
	inBlock, inHTML := false, false
	for li, line := range lines {
		var b strings.Builder
		var quote byte
		for i := 0; i < len(line); i++ {
			c := line[i]
			switch {
			case inBlock:
				if c == '*' && i+1 < len(line) && line[i+1] == '/' {
					inBlock = false
					i++
				}
			case inHTML:
				if strings.HasPrefix(line[i:], "-->") {
					inHTML = false
					i += 2
				}
			case quote != 0:
				if c == '\\' {
					i++
				} else if c == quote {
					quote = 0
				}
			case c == '/' && i+1 < len(line) && line[i+1] == '*':
				inBlock = true
				i++
			case c == '/' && i+1 < len(line) && line[i+1] == '/':
				i = len(line)
			case strings.HasPrefix(line[i:], "<!--"):
				inHTML = true
				i += 3
			case c == '\'' || c == '"' || c == '`':
				quote = c
			default:
				b.WriteByte(c)
			}
		}
		out[li] = b.String()
	}
	return out
}
