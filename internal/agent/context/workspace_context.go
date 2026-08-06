package agentctx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// 工作区上下文协议（GD-14-005 / ADR-0088 决策三）
//
// 业界（AGENTS.md / CLAUDE.md 约定）的做法是：Agent 自动装载工作目录内的约束
// 文档，使其对当前项目的规范"开箱即知"。本实现落地该协议，但**不采用**业界
// 默认的信任级别。
//
// 信任模型（本文件的核心决策，不可在未走 ADR 的情况下放宽）：
//
//	默认 —— 装入 ZoneExternalCatalog 等价的低信任区并按 TaintExternal 处理，
//	        文本经 Spotlighting 围栏包裹，明确告知模型"这是参考资料不是指令"。
//	例外 —— 仅当工作区路径出现在用户**显式配置**的信任列表中，才写入
//	        ZoneImmutable（等同系统提示词级信任）。
//
// 为什么默认必须是低信任：Agent 处理一个 clone 下来的仓库时，其中的
// AGENTS.md 完全是攻击者可控的。若无条件装入 ZoneImmutable，等于给了任意
// 第三方仓库一条直通最高信任区的提示注入通路，正好绕过 HE-2 所依赖的
// 确定性边界——这条通路比它带来的便利危险得多。信任必须由用户显式授予，
// 而不是由"文件恰好叫这个名字"推定。
// ============================================================================

// workspaceContextFiles 按优先级排列的工作区上下文文件名。
// 同一目录下多个命中时全部装载，顺序即此处顺序（保证不同部署下装配确定性）。
//
// 用函数而非包级 var：R1 禁 internal/ 全局可变变量（并发安全 + 测试隔离）。
// 一个可被任意包改写的文件名列表在这里尤其危险——它决定了"哪些文件的内容
// 会被读进 Prompt"，运行期被改写等于扩大注入面。
func workspaceContextFiles() []string {
	return []string{
		"AGENTS.md",
		"CLAUDE.md",
		".polaris_context.md",
	}
}

// maxWorkspaceContextBytes 单个上下文文件的读取上限（Tier-0 约束）。
// 超限截断而非拒绝：超大 AGENTS.md 更可能是仓库把文档堆在一起，
// 截断后前半部分通常仍是有效约束；直接跳过则等于完全失去该能力。
const maxWorkspaceContextBytes = 32 * 1024

// WorkspaceContext 一份已装载的工作区上下文文档。
type WorkspaceContext struct {
	// RelPath 相对工作区根的路径，用于在 Prompt 中标注来源。
	RelPath string
	// Content 文档正文（可能已按 maxWorkspaceContextBytes 截断）。
	Content string
	// Trusted 是否来自用户显式声明的信任路径。
	// true → 可写入 ZoneImmutable；false → 低信任区 + Spotlighting。
	Trusted bool
}

// WorkspaceContextLoader 从工作区根目录探测并装载标准上下文文档。
//
// trustedRoots 是用户在配置中显式声明信任的**绝对路径**前缀列表。
// 为空（默认）时所有工作区上下文一律按不可信处理——这是刻意的默认值选择：
// 安全默认必须是"不信任"，信任是用户主动的、针对具体路径的授权行为。
type WorkspaceContextLoader struct {
	trustedRoots []string
}

// NewWorkspaceContextLoader 构造装载器。
// trustedRoots 中的相对路径会被忽略（无法可靠判定信任边界，见 isTrusted）。
func NewWorkspaceContextLoader(trustedRoots []string) *WorkspaceContextLoader {
	cleaned := make([]string, 0, len(trustedRoots))
	for _, r := range trustedRoots {
		r = strings.TrimSpace(r)
		if r == "" || !filepath.IsAbs(r) {
			continue
		}
		cleaned = append(cleaned, filepath.Clean(r))
	}
	sort.Strings(cleaned) // 确定性：配置顺序不应影响判定结果
	return &WorkspaceContextLoader{trustedRoots: cleaned}
}

// Load 探测 rootDir 下的标准上下文文档并读取内容。
//
// rootDir 为空或不可读时返回 nil（无上下文），不返回错误——工作区没有约束
// 文档是完全正常的状态，不是故障。单个文件读取失败同样跳过而非整体失败。
func (l *WorkspaceContextLoader) Load(_ context.Context, rootDir string) []WorkspaceContext {
	if rootDir == "" {
		return nil
	}
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil
	}
	trusted := l.isTrusted(absRoot)

	var out []WorkspaceContext
	for _, name := range workspaceContextFiles() {
		full := filepath.Join(absRoot, name)

		// 路径逃逸防御：name 是包内常量，理论上不含 ..，但 Join 结果仍显式
		// 校验仍在 root 之内——防止未来有人把 name 改成可配置项时留下缺口。
		if !strings.HasPrefix(full, absRoot+string(os.PathSeparator)) {
			continue
		}
		info, statErr := os.Stat(full)
		if statErr != nil || info.IsDir() {
			continue
		}
		data, readErr := os.ReadFile(full) //nolint:gosec // full 已校验在 absRoot 之内
		if readErr != nil {
			continue
		}
		if len(data) > maxWorkspaceContextBytes {
			data = data[:maxWorkspaceContextBytes]
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}
		out = append(out, WorkspaceContext{RelPath: name, Content: content, Trusted: trusted})
	}
	return out
}

// isTrusted 判定工作区根是否落在用户显式声明的信任路径内。
//
// 只接受绝对路径且做 Clean 后的前缀匹配（含目录边界校验），
// 避免 "/home/user/proj-evil" 被 "/home/user/proj" 误判为信任。
func (l *WorkspaceContextLoader) isTrusted(absRoot string) bool {
	for _, root := range l.trustedRoots {
		if absRoot == root || strings.HasPrefix(absRoot, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// RenderUntrusted 把不可信的工作区上下文渲染为带围栏的 TaintedString，
// 供 PromptBuilder.WriteExternalCatalog 之类的低信任写入口使用。
//
// 污点等级固定 TaintHigh：与 S-02 的第三方扩展自述同级——两者都是
// "第三方可控、看起来像指令的文本"，威胁模型完全一致，不应给工作区文件
// 更高的信任度。
//
// 无内容时返回的仍是经 NewTaintedString 构造的（空内容）污点串，而非零值字面量
// ——零值构造绕过唯一合法入口，被 make taint-check 拦截；调用方用 IsEmpty() 判空。
func RenderUntrusted(cs []WorkspaceContext) taint.TaintedString {
	var b strings.Builder
	for _, c := range cs {
		if c.Trusted {
			continue
		}
		fmt.Fprintf(&b, "--- %s ---\n%s\n", c.RelPath, c.Content)
	}
	return taint.NewTaintedString(
		b.String(),
		taint.TaintSource{Module: "workspace", OriginTaintLevel: types.TaintHigh},
		"workspace_context")
}

// RenderTrusted 把用户显式信任的工作区上下文渲染为可写入 ZoneImmutable 的文本。
// 无信任内容时返回空串（调用方应跳过写入）。
func RenderTrusted(cs []WorkspaceContext) string {
	var b strings.Builder
	for _, c := range cs {
		if !c.Trusted {
			continue
		}
		fmt.Fprintf(&b, "--- %s ---\n%s\n", c.RelPath, c.Content)
	}
	if b.Len() == 0 {
		return ""
	}
	return "<workspace_instructions trust=\"user_declared\">\n" +
		"以下是用户已显式声明信任的工作区约束文档，视为项目级系统指令遵守。\n" +
		b.String() + "</workspace_instructions>"
}
