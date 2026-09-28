package webui

import _ "embed"

// MCPAppsSandboxHTML 是 MCP Apps Sandbox proxy 页面（M8f-1，与 dist/ 编译产物
// 分开嵌入——它是独立静态页面，不经 Vite 打包，由 internal/gateway/server 的
// 沙箱专用监听器直接served）。内容占位，8f-2 替换为真正的双 iframe 代理实现。
//
//go:embed src/mcp-apps/sandbox.html
var MCPAppsSandboxHTML string
