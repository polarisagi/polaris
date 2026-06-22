package configs

import "embed"

// FS 嵌入所有内置配置文件，使二进制完全自包含、不依赖工作目录。
//
// 嵌入范围：
//   - *.toml：根目录配置（defaults）
//   - extensions/：生态配置文件（marketplaces, registry, trusted-publishers, automation_sources）
//   - prompts/：Kernel Prompt 模板
//   - automations/：内置自动化模板（builtin.yaml）
//   - policy/：Cedar 策略文件（hard_constraints.cedar / soft_constraints.cedar / memory.cedar）
//
// 排除（设计意图）：
//   - threshold-examples/：仅供 Operator 复制到 ~/.polarisagi/polaris/config/ 使用，不嵌入
//
//go:embed *.toml prompts automations extensions policy
var FS embed.FS
