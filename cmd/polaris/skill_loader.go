// skill_loader.go — 启动时将 DB 中已有技能批量同步到运行时注册表。
//
// loadSkillsToToolRegistry：
//
//	非致命，单个 skill 失败仅记录 WARN，不阻断启动。
//	注册的 InProcessFn 委托至 skill.ScriptSkillExecutor.ExecuteSkill（唯一实现），
//	不在此处重复渲染 instructions / 执行脚本逻辑——避免与 Dispatcher/Agent FastPath 产生第二套实现。
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	polartool "github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/pkg/types"
)

// parseDescription 从 instructions 或 capabilities JSON 数组中提取工具描述。
// capabilities 格式：["description:xxx", "tag:yyy", ...]，优先取 description: 前缀项。
// fallback：截取 instructions 前 200 字符。
func parseDescription(instructions, capsRaw string) string {
	var caps []string
	if json.Unmarshal([]byte(capsRaw), &caps) == nil {
		for _, c := range caps {
			if d, ok := strings.CutPrefix(c, "description:"); ok {
				return d
			}
		}
	}
	if len(instructions) > 0 {
		desc := instructions
		if len(desc) > 200 {
			desc = desc[:200] + "…"
		}
		return desc
	}
	return ""
}

// loadSkillsToToolRegistry 启动时将 DB 中 runtime='script' exec_mode='tool' 的技能
// 批量同步到 InMemoryToolRegistry 和 InProcessSandbox，
// 使 Agent Kernel FSM 在系统已安装的技能上具备可发现能力。
// skillExec 非 nil 时，注册的执行函数委托至其 ExecuteSkill（唯一实现：instructions 渲染 /
// Logic Collapse 脚本执行 / PolicyGate 校验均在 internal/extension/skill.ScriptSkillExecutor 完成）。
func loadSkillsToToolRegistry(ctx context.Context, db protocol.SQLQuerier, toolReg *polartool.InMemoryToolRegistry, sbx *sandbox.InProcessSandbox, skillExec protocol.SkillExecutor) { //nolint:gocyclo
	if db == nil || toolReg == nil || sbx == nil {
		return
	}
	rows, err := db.QueryContext(ctx,
		`SELECT name, instructions, capabilities FROM skills WHERE runtime='script' AND exec_mode='tool' AND deprecated=0`)
	if err != nil {
		slog.Warn("loadSkillsToToolRegistry: query failed", "err", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var dbName, instructions, capsRaw string
		if rows.Scan(&dbName, &instructions, &capsRaw) != nil {
			continue
		}
		slug, ok := strings.CutPrefix(dbName, "skill:")
		if !ok || slug == "" {
			continue
		}
		llmName := "skill__" + slug

		desc := parseDescription(instructions, capsRaw)

		// 注册 InProcessFn：委托至 skillExec.ExecuteSkill（唯一实现）。
		// skillExec 为 nil 时（理论上不应发生，boot_tools.go 始终构造）降级为直接返回启动时快照的 instructions，
		// 不重新实现 DB 重新加载 / 输入拼接逻辑。
		capturedSlug := slug
		capturedInst := instructions
		sbx.Register(llmName, func(ctx context.Context, input []byte) ([]byte, error) {
			if skillExec != nil {
				return skillExec.ExecuteSkill(ctx, "skill:"+capturedSlug, input)
			}
			return []byte(capturedInst), nil
		})

		if regErr := toolReg.Register(types.Tool{
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
			slog.Warn("loadSkillsToToolRegistry: register failed", "skill", llmName, "err", regErr)
			continue
		}
		count++
	}
	if err := rows.Err(); err != nil {
		slog.Warn("loadSkillsToToolRegistry: rows iteration error", "err", err)
	}
	if count > 0 {
		slog.Info("polaris: loaded skills from DB to InMemoryToolRegistry", "count", count)
	}
}
