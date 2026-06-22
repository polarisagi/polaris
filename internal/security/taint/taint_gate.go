package taint

import (
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// TaintGate — Prompt Slot 物理隔离检查（M11 §2.1 D1 防线）。
// 架构文档: docs/arch/M11-Policy-Safety.md §2
//
// 防线原则：data / tool_result 槽的内容（TaintMedium+）绝对不得流入 instruction 槽。
// 任何违规立即返回 ErrTaintSlotViolation，fail-closed。

// PromptSlot 标识 Prompt 的内容槽位。
type PromptSlot string

const (
	SlotSystem      PromptSlot = "system"      // TaintNone，编译期常量
	SlotInstruction PromptSlot = "instruction" // TaintLow，用户输入
	SlotData        PromptSlot = "data"        // TaintHigh，不可信外部数据
	SlotToolResult  PromptSlot = "tool_result" // TaintHigh，工具输出
	SlotEgDemo      PromptSlot = "eg_demo"     // TaintNone，示例
)

// slotMaxAllowedTaint 定义各槽位允许注入的最高污点等级（含边界，只读）。
func slotMaxAllowedTaint() map[PromptSlot]types.TaintLevel {
	return map[PromptSlot]types.TaintLevel{
		SlotSystem:      types.TaintNone,
		SlotInstruction: types.TaintLow,
		SlotData:        types.TaintHigh, // data 槽接受高污点
		SlotToolResult:  types.TaintHigh, // tool_result 同上
		SlotEgDemo:      types.TaintNone,
	}
}

// ErrTaintSlotViolation 污点等级违反 Slot 物理隔离约束。
var ErrTaintSlotViolation = apperr.ErrTaintViolation

// TaintGate 是 Prompt 组装时的污点门控。
// 调用方（kernel.PromptBuilder.WriteUserData）在写入 ZoneTaintedData 前调用 CheckSlotAssignment。
type TaintGate struct{}

// CheckSlotAssignment 检查将指定污点等级的内容写入目标槽位是否合规。
// 违规 → ErrTaintSlotViolation（fail-closed，调用方必须中止组装）。
func (g *TaintGate) CheckSlotAssignment(slot PromptSlot, level types.TaintLevel) error {
	maxAllowed, ok := slotMaxAllowedTaint()[slot]
	if !ok {
		// 未知槽位，fail-closed
		return ErrTaintSlotViolation
	}
	if level > maxAllowed {
		return ErrTaintSlotViolation
	}
	return nil
}

// CheckMultiSource 计算多个输入的合并污点（PropagateTaint 语义），
// 再检查合并结果是否可以进入目标槽位。
func (g *TaintGate) CheckMultiSource(slot PromptSlot, levels ...types.TaintLevel) error {
	combined := types.PropagateTaint(levels...)
	return g.CheckSlotAssignment(slot, combined)
}
