package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

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
You must use the '@modelcontextprotocol/sdk' package to define the server.

Output ONLY valid JSON matching this schema:
{
  "name": "plugin-name",
  "description": "What this plugin does...",
  "typescript_code": "import { McpServer } from \"@modelcontextprotocol/sdk/server/mcp.js\";\nimport { StdioServerTransport } from \"@modelcontextprotocol/sdk/server/stdio.js\";\nimport { z } from \"zod\";\n\nconst server = new McpServer({ name: \"plugin-name\", version: \"1.0.0\" });\n\nserver.tool(\"my_tool\", \"Description\", {}, async () => ({\n  content: [{ type: \"text\", text: \"Done\" }],\n}));\n\nconst transport = new StdioServerTransport();\nawait server.connect(transport);\n"
}
Do not include any Markdown wrappers like ` + "```json" + ` in the output. Ensure the TypeScript code is properly escaped in the JSON string.
`

// GeneratePlugin takes a user's intent, calls the LLM, and creates the physical plugin directory, .mcp.json, and server.py.
func (c *PluginCreator) GeneratePlugin(ctx context.Context, intent string) (string, error) {
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

	// Write package.json（npx tsx 无需编译步骤，直接运行 TypeScript）
	packageJSON := fmt.Sprintf(`{
  "name": "%s",
  "version": "1.0.0",
  "description": "%s",
  "type": "module",
  "scripts": {
    "start": "npx tsx src/index.ts"
  },
  "dependencies": {
    "@modelcontextprotocol/sdk": "^1.0.0",
    "zod": "^3.0.0"
  },
  "devDependencies": {
    "@types/node": "^20.0.0",
    "typescript": "^5.0.0",
    "tsx": "^4.0.0"
  }
}`, result.Name, result.Description)

	packageJSONPath := filepath.Join(pluginDir, "package.json")
	if err := os.WriteFile(packageJSONPath, []byte(packageJSON), 0644); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "plugin_creator: failed to write package.json", err)
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

	// Create .mcp.json（使用 npx tsx 直接运行 TypeScript，无需预编译）
	mcpJSON := fmt.Sprintf(`{
  "mcpServers": {
    "%s": {
      "command": "npx",
      "args": ["tsx", "src/index.ts"]
    }
  }
}`, result.Name)

	mcpJSONPath := filepath.Join(pluginDir, ".mcp.json")
	if err := os.WriteFile(mcpJSONPath, []byte(mcpJSON), 0644); err != nil {
		return "", perrors.Wrap(perrors.CodeInternal, "plugin_creator: failed to write .mcp.json", err)
	}

	return pluginDir, nil
}

func extractJSON(input string) string {
	re := regexp.MustCompile(`(?s)\{.*\}`)
	match := re.FindString(input)
	if match != "" {
		return match
	}
	return input
}
