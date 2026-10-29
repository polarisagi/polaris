package webui

import _ "embed"

// MCPAppsSandboxHTML 是 MCP Apps Sandbox proxy 页面（M8f，与 dist/ 编译产物
// 分开嵌入——它是独立静态页面，不经 Vite 打包，由 internal/gateway/server 的
// 沙箱专用监听器直接 served）。
//
//go:embed src/mcp-apps/sandbox.html
var MCPAppsSandboxHTML string

// MCPAppsOpenAIShim 是 window.openai 兼容垫片脚本，由服务端注入每个 View 文档的
// <head> 最前（ADR-0126）。与 Vite 打包无关，故同样独立嵌入。
//
//go:embed src/mcp-apps/openai_shim.js
var MCPAppsOpenAIShim string
