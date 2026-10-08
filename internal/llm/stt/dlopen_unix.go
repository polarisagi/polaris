//go:build !windows

package stt

import (
	"github.com/ebitengine/purego"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Dlopen 平台安全地加载动态库。
//
// 用 RTLD_LOCAL 而不是 RTLD_GLOBAL：进程内同时存在两份不同版本的 ORT（语音自带 1.28.2，
// 向量化引擎独立钉 1.23.2，ADR-0109）。ELF 下 RTLD_GLOBAL 会把先加载那份的 OrtGetApiBase 等符号
// 放进全局作用域，后加载的库按符号插入规则也绑定到它，版本不符时 OrtGetApi 返回 NULL 而崩溃。
// RTLD_LOCAL 让各库只在自己的依赖作用域内解析；purego 取函数用 dlsym(handle)，不依赖全局作用域。
func Dlopen(abs string) (uintptr, error) {
	h, err := purego.Dlopen(abs, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "stt: dlopen 失败: "+abs, err)
	}
	return h, nil
}
