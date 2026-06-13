package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// denoPermFlags 根据信任等级返回 Deno 运行时权限标志。
// 对应 extension_instances.trust_tier 的语义：
//
//	1 = user（用户手动创建）    → 最小权限，仅允许出站网络 + 环境变量读取
//	2 = learned（M9 自演化）    → 与 user 相同
//	3 = marketplace（市场安装） → 允许出站网络 + env + 有限只读文件
//	4 = builtin（内置）         → 全权限（仅内置插件使用）
func denoPermFlags(trustTier int) []string {
	switch {
	case trustTier >= 4:
		// 内置插件：全权限
		return []string{"--allow-all"}
	case trustTier == 3:
		// 市场插件：网络 + env + 只读（不允许写文件、不允许子进程）
		return []string{
			"--allow-net",
			"--allow-env",
			"--allow-read",
		}
	default:
		// user / learned：最小权限，仅出站网络 + env（无文件 IO，无子进程）
		return []string{
			"--allow-net",
			"--allow-env",
		}
	}
}

func checkDenoAvailable() bool {
	cmd := exec.Command("deno", "--version")
	return cmd.Run() == nil
}

// LLMClient is a minimal interface for the PluginCreator to generate responses.
type LLMClient interface {
	// Generate uses the system prompt and user intent to generate a structured response.
	Generate(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// PluginCreator defines the auto-generation workflow for MCP plugins based on user intent.
type PluginCreator struct {
	llm     LLMClient
	baseDir string // e.g. ~/.polarisagi/polaris/extensions/local/
}

// NewPluginCreator initializes a new creator for auto-generating plugins.
func NewPluginCreator(llm LLMClient, baseDir string) *PluginCreator {
	return &PluginCreator{
		llm:     llm,
		baseDir: baseDir,
	}
}

// GeneratedPlugin represents the structured output expected from the LLM.
type GeneratedPlugin struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	TypeScriptCode string `json:"typescript_code"`
}

const pluginCreatorSystemPrompt = `
You are the internal plugin-creator agent. Your job is to translate a user's intent into a fully functional MCP (Model Context Protocol) plugin using TypeScript.
A plugin MUST have a concise name (kebab-case) and a clear description.
You must use the 'npm:@modelcontextprotocol/sdk' package to define the server. The plugin will run on the Deno runtime, so use 'npm:' prefix for Node.js packages.

Output ONLY valid JSON matching this schema:
{
  "name": "plugin-name",
  "description": "What this plugin does...",
  "typescript_code": "import { McpServer } from \"npm:@modelcontextprotocol/sdk/server/mcp.js\";\nimport { StdioServerTransport } from \"npm:@modelcontextprotocol/sdk/server/stdio.js\";\nimport { z } from \"npm:zod\";\n\nconst server = new McpServer({ name: \"plugin-name\", version: \"1.0.0\" });\n\nserver.tool(\"my_tool\", \"Description\", {}, async () => ({\n  content: [{ type: \"text\", text: \"Done\" }],\n}));\n\nconst transport = new StdioServerTransport();\nawait server.connect(transport);\n"
}
Do not include any Markdown wrappers like ` + "```json" + ` in the output. Ensure the TypeScript code is properly escaped in the JSON string.
`

// GeneratePlugin takes a user's intent, calls the LLM, and creates the physical plugin directory, .mcp.json, and server.py.
func (c *PluginCreator) GeneratePlugin(ctx context.Context, intent string, trustTier int) (string, error) {
	if c.llm == nil {
		return "", perrors.New(perrors.CodeInternal, "plugin_creator: LLM client is nil")
	}

	response, err := c.llm.Generate(ctx, pluginCreatorSystemPrompt, intent)
	if err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "plugin_creator: failed to generate plugin", err)
	}

	// Simple JSON extraction to handle model quirks
	jsonStr := extractJSON(response)

	var result GeneratedPlugin
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "plugin_creator: failed to parse generated plugin JSON", err)
	}

	if result.Name == "" || result.Description == "" || result.TypeScriptCode == "" {
		return "", perrors.New(perrors.CodeInternal, "plugin_creator: invalid generation, missing required fields")
	}

	// Create physical directory structure
	pluginDir := filepath.Join(c.baseDir, result.Name)
	srcDir := filepath.Join(pluginDir, "src")

	if err := os.MkdirAll(srcDir, 0755); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "plugin_creator: failed to create src directory", err)
	}

	// Write src/index.ts
	indexTSPath := filepath.Join(srcDir, "index.ts")
	if err := os.WriteFile(indexTSPath, []byte(result.TypeScriptCode), 0644); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "plugin_creator: failed to write src/index.ts", err)
	}

	// Write deno.json (Deno config, no node_modules required)
	denoConfigPath := filepath.Join(pluginDir, "deno.json")
	denoConfig := fmt.Sprintf(`{
  "name": "%s",
  "version": "1.0.0",
  "imports": {
    "@modelcontextprotocol/sdk": "npm:@modelcontextprotocol/sdk@^1.0.0",
    "zod": "npm:zod@^3.0.0"
  }
}
`, result.Name)
	if err := os.WriteFile(denoConfigPath, []byte(denoConfig), 0644); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "plugin_creator: failed to write deno.json", err)
	}

	// Create a default plugin.json
	pluginMetaDir := filepath.Join(pluginDir, ".polaris-plugin")
	if err := os.MkdirAll(pluginMetaDir, 0755); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "plugin_creator: failed to create .polaris-plugin directory", err)
	}

	pluginJSON := fmt.Sprintf(`{
  "name": "%s",
  "version": "1.0.0",
  "description": "%s",
  "mcpServers": "./.mcp.json"
}`, result.Name, result.Description)

	pluginJSONPath := filepath.Join(pluginMetaDir, "plugin.json")
	if err := os.WriteFile(pluginJSONPath, []byte(pluginJSON), 0644); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "plugin_creator: failed to write plugin.json", err)
	}

	var cmd string
	var argsJSON string

	if checkDenoAvailable() {
		// Deno available
		permFlags := denoPermFlags(trustTier)
		denoArgs := append([]string{"run", "--no-prompt"}, permFlags...)
		denoArgs = append(denoArgs, "src/index.ts")
		cmd = "deno"
		argsJSON = marshalArgs(denoArgs)
	} else {
		slog.Warn("plugin_creator: deno not found, falling back to npx tsx (no sandbox restrictions)")
		cmd = "npx"
		argsJSON = `["tsx", "src/index.ts"]`
	}

	// Create .mcp.json
	mcpJSON := fmt.Sprintf(`{
  "mcpServers": {
    "%s": {
      "command": "%s",
      "args": %s
    }
  }
}`, result.Name, cmd, argsJSON)

	mcpJSONPath := filepath.Join(pluginDir, ".mcp.json")
	if err := os.WriteFile(mcpJSONPath, []byte(mcpJSON), 0644); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "plugin_creator: failed to write .mcp.json", err)
	}

	return pluginDir, nil
}

func marshalArgs(args []string) string {
	b, _ := json.Marshal(args)
	return string(b)
}

func extractJSON(input string) string {
	re := regexp.MustCompile(`(?s)\{.*\}`)
	match := re.FindString(input)
	if match != "" {
		return match
	}
	return input
}
