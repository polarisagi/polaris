// runtime_registrar.go — runtimeRegistrarAdapter：运行时扩展注册器。
//
// 实现 marketplace.RuntimeRegistrar 接口，在扩展安装完成后将其接入运行时：
//   - "skill" → SkillRegistry + InMemoryToolRegistry + InProcessSandbox
//   - "mcp"   → MCPManager
//   - 其他    → 跳过（plugin/app 由 native.RegisterExtensionTools 处理）
//
// 在 cmd/ 层定义以避免 marketplace → skill/mcp/tool 包循环。
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	polartool "github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// runtimeRegistrarAdapter 将已安装扩展注册到运行时组件。
// installMgr 在第 420 行创建时 skillRegistry 还未初始化，
// 构造完成后通过 installMgr.WithRegistrar 热注入（M13-bis P1-FIX）。
type runtimeRegistrarAdapter struct {
	skillRegistry protocol.SkillRegistry
	mcpMgr        *mcp.MCPManager
	toolReg       *polartool.InMemoryToolRegistry // 安装时同步技能到 InMemoryToolRegistry，使 Agent FSM 可发现
	inProcSandbox *sandbox.InProcessSandbox       // 安装时注册 skill InProcessFn 到 InProcess 执行路径
	db            protocol.SQLQuerier             // 查询 skill instructions（skill InProcessFn 运行时所需）
}

// Register 根据 extType 将已安装扩展注册到对应运行时。
// extType: "skill" → SkillRegistry；"mcp" → MCPManager；其他记日志跳过。
// installDir 为空时静默降级（Day0-ColdStart）。
func (r *runtimeRegistrarAdapter) Register(ctx context.Context, extType, installDir, instID string) error {
	if installDir == "" {
		// installDir 为空说明是纯 DB 类型扩展（如 plugin 元数据），不需要运行时注册
		slog.Info("runtime_registrar: no installDir, skipping runtime registration",
			"ext_type", extType, "inst_id", instID)
		return nil
	}

	switch extType {
	case "skill":
		return r.registerSkill(ctx, installDir, instID)
	case "mcp":
		return r.registerMCP(ctx, installDir, instID)
	default:
		// plugin / app / unknown：不需要运行时注册（由 native.RegisterExtensionTools 处理）
		slog.Debug("runtime_registrar: ext_type has no runtime registration path",
			"ext_type", extType, "inst_id", instID)
		return nil
	}
}

// registerSkill 读取 installDir 下的 manifest，注册到 SkillRegistry。
func (r *runtimeRegistrarAdapter) registerSkill(ctx context.Context, installDir, instID string) error {
	if r.skillRegistry == nil {
		return nil
	}

	// 读取 SKILL.md（若存在）作为 instructions
	var instructions string
	if raw, err := os.ReadFile(filepath.Join(installDir, "SKILL.md")); err == nil {
		instructions = string(raw)
	}

	// 脚本入口：优先 src/index.ts，其次 index.ts，最后 src/index.js / index.js
	scriptPath := ""
	for _, candidate := range []string{"src/index.ts", "index.ts", "src/index.js", "index.js"} {
		full := filepath.Join(installDir, candidate)
		if _, statErr := os.Stat(full); statErr == nil {
			scriptPath = full
			break
		}
	}
	if scriptPath == "" {
		slog.Warn("runtime_registrar: skill has no entry script, skip",
			"inst_id", instID, "install_dir", installDir)
		return nil
	}

	// 从 instID 推导 skill 名称（ext_xxxxxxxx → 去前缀）
	skillName := strings.TrimPrefix(instID, "ext_")

	meta := types.SkillMeta{
		Name:         skillName,
		Version:      "1.0.0",
		Runtime:      "script",
		RiskLevel:    "medium",
		Sandbox:      3, // Container 隔离（外部安装的 Skill 默认高隔离）
		ExecMode:     "tool",
		Trust:        types.TrustCommunity,
		ScriptPath:   scriptPath,
		Instructions: instructions,
	}

	if err := r.skillRegistry.Register(ctx, meta); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "runtime_registrar.registerSkill", err)
	}

	// 同步到 InMemoryToolRegistry，使 Agent Kernel FSM 可通过 toolReg.List() 发现并调用此技能。
	// skill 执行语义与 Gateway toolExec 路径一致：返回 instructions + 用户输入文本。
	r.syncSkillToToolRegistry(skillName, meta.Instructions)

	slog.Info("runtime_registrar: skill registered to SkillRegistry",
		"skill_name", skillName, "inst_id", instID, "script", scriptPath)
	return nil
}

// syncSkillToToolRegistry 将已注册的 skill 同步到 InMemoryToolRegistry 和 InProcessSandbox，
// 使 Agent Kernel FSM 可通过工具调用路径发现并执行该技能。
func (r *runtimeRegistrarAdapter) syncSkillToToolRegistry(skillName string, instructions string) {
	if r.toolReg == nil || r.inProcSandbox == nil {
		return
	}
	llmName := "skill__" + skillName
	db := r.db
	r.inProcSandbox.Register(llmName, func(ctx context.Context, input []byte) ([]byte, error) {
		// 运行时重查 instructions（优先 DB 最新值，fallback 启动时快照）
		var dbInst string
		if db != nil {
			_ = db.QueryRowContext(ctx,
				`SELECT instructions FROM skills WHERE name=? AND deprecated=0`, "skill:"+skillName).Scan(&dbInst)
		}
		inst := dbInst
		if inst == "" {
			inst = instructions
		}
		var req struct {
			Input string `json:"input"`
		}
		_ = json.Unmarshal(input, &req)
		out := inst
		if req.Input != "" {
			out += "\n\n---\n\n输入：" + req.Input
		}
		return []byte(out), nil
	})
	desc := instructions
	if len(desc) > 200 {
		desc = desc[:200] + "…"
	}
	if regErr := r.toolReg.Register(types.Tool{
		Name:        llmName,
		Description: desc,
		Source:      types.ToolSkill,
		RiskLevel:   types.RiskMedium,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{"type": "string", "description": "任务描述或输入内容"},
			},
			"required": []string{"input"},
		},
	}); regErr != nil {
		slog.Warn("runtime_registrar: skill sync to toolReg failed", "skill", skillName, "err", regErr)
	}
}

// registerMCP 读取 installDir/mcp.json，注册到 MCPManager。
func (r *runtimeRegistrarAdapter) registerMCP(ctx context.Context, installDir, instID string) error {
	if r.mcpMgr == nil {
		return nil
	}

	// 读取 mcp.json 配置（最小集格式）
	cfgPath := filepath.Join(installDir, "mcp.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return apperr.Wrap(apperr.CodeInternal, "runtime_registrar.registerMCP: read mcp.json", err)
		}
		// mcp.json 缺失属合法场景：MCP 扩展可能由 DB mcp_servers 表的 LoadFromDB 路径加载
		slog.Warn("runtime_registrar: mcp.json not found, skip runtime registration",
			"inst_id", instID, "path", cfgPath)
		return nil //nolint:nilerr // os.ErrNotExist 为预期降级路径，不作错误传播
	}

	// mcp.json 格式（最小集）：
	// {"name":"my-server","transport":"stdio","command":"npx","args":["-y","my-mcp"]}
	// HTTP 模式：{"name":"srv","transport":"streamable_http","endpoint":"http://..."}
	var mcpCfg struct {
		Name      string            `json:"name"`
		Transport string            `json:"transport"`
		Command   string            `json:"command"`
		Args      []string          `json:"args"`
		Endpoint  string            `json:"endpoint"` // 映射到 MCPClientConfig.URL
		Env       map[string]string `json:"env"`
	}
	if jsonErr := json.Unmarshal(raw, &mcpCfg); jsonErr != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "runtime_registrar.registerMCP: parse mcp.json", jsonErr)
	}

	name := mcpCfg.Name
	if name == "" {
		name = strings.TrimPrefix(instID, "ext_")
	}

	clientCfg := mcp.MCPClientConfig{
		Transport: mcp.MCPTransport(mcpCfg.Transport),
		Command:   mcpCfg.Command,
		Args:      mcpCfg.Args,
		URL:       mcpCfg.Endpoint, // mcp.json 用 endpoint，MCPClientConfig 字段名为 URL
		Env:       mcpCfg.Env,
	}

	if addErr := r.mcpMgr.Add(ctx, instID, name, clientCfg); addErr != nil {
		return apperr.Wrap(apperr.CodeInternal, "runtime_registrar.registerMCP", addErr)
	}

	slog.Info("runtime_registrar: MCP server registered to MCPManager",
		"inst_id", instID, "name", name, "transport", mcpCfg.Transport)
	return nil
}
