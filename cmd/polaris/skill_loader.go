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

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	polartool "github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

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
		`SELECT name, instructions, capabilities, description, display_name, model_invocable, spec
		 FROM skills WHERE runtime='script' AND exec_mode='tool' AND deprecated=0`)
	if err != nil {
		slog.Warn("loadSkillsToToolRegistry: query failed", "err", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var meta types.SkillMeta
		var capsRaw string
		var modelInvocable int
		if rows.Scan(&meta.Name, &meta.Instructions, &capsRaw, &meta.Description, &meta.DisplayName, &modelInvocable, &meta.Spec) != nil {
			continue
		}
		meta.DisableModelInvocation = modelInvocable == 0
		if err := json.Unmarshal([]byte(capsRaw), &meta.Capabilities); err != nil {
			slog.Warn("loadSkillsToToolRegistry: corrupt capabilities", "skill", meta.Name, "err", err)
		}
		// 与 SkillCatalog 共用同一视图：模型不可调用的技能不注册为工具。
		view, ok := catalog.ModelToolView(meta)
		if !ok {
			continue
		}

		// 注册 InProcessFn：委托至 skillExec.ExecuteSkill（唯一实现）。
		// skillExec 为 nil 时（理论上不应发生，boot_tools.go 始终构造）降级为直接返回启动时快照的 instructions。
		skillName, snapshot := meta.Name, meta.Instructions
		sbx.Register(view.Name, func(ctx context.Context, input []byte) ([]byte, error) {
			if skillExec != nil {
				return skillExec.ExecuteSkill(ctx, skillName, input)
			}
			return []byte(snapshot), nil
		})

		if regErr := toolReg.Register(types.Tool{
			Name:        view.Name,
			Description: view.Description,
			Source:      types.ToolSkill,
			RiskLevel:   types.RiskMedium,
			InputSchema: view.InputSchema,
		}); regErr != nil {
			slog.Warn("loadSkillsToToolRegistry: register failed", "skill", view.Name, "err", regErr)
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
