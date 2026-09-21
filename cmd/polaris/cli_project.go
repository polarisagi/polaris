// polaris project 子命令组 + chat 的项目解析（ADR-0097 决策四，ADR-0096 决策一）。
//
// 纯 HTTP 客户端：root_path 规范化、信任写入面收窄、默认项目保护全在守护进程侧，
// CLI 不重复实现任何一条——两份实现只会各自漂移。CLI 独有的只有"按当前目录匹配项目"
// 这一条语法糖：它依赖的是调用方进程的 cwd，守护进程拿不到。
package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// cliProject /v1/projects 的响应行（仅 CLI 用到的字段）。
type cliProject struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	RootPath     string `json:"root_path"`
	Trusted      bool   `json:"trusted"`
	Archived     bool   `json:"archived"`
	IsDefault    bool   `json:"is_default"`
	SessionCount int    `json:"session_count"`
}

func cliListProjects(includeArchived bool) ([]cliProject, error) {
	path := "/v1/projects"
	if includeArchived {
		path += "?include_archived=true"
	}
	var resp struct {
		Projects []cliProject `json:"projects"`
	}
	if err := cliRequest("GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Projects, nil
}

// canonicalDir 与守护进程 normalizeProjectRoot 同口径（Abs + EvalSymlinks），否则
// macOS 上 /tmp 与 /private/tmp 这类软链会让前缀比较永远失配。
func canonicalDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// matchProjectByDir 返回 root_path 包含 dir 的未归档项目；多个命中取最长 root（最内层）。
// dir 须已是规范路径。无命中返回 nil。
func matchProjectByDir(projects []cliProject, dir string) *cliProject {
	var best *cliProject
	for i := range projects {
		p := &projects[i]
		if p.Archived || p.RootPath == "" {
			continue
		}
		if dir != p.RootPath && !strings.HasPrefix(dir, p.RootPath+string(os.PathSeparator)) {
			continue
		}
		if best == nil || len(p.RootPath) > len(best.RootPath) {
			best = p
		}
	}
	return best
}

// resolveProjectRef 按 ID 精确匹配，其次按名称精确匹配（须唯一）。
func resolveProjectRef(projects []cliProject, ref string) (*cliProject, error) {
	for i := range projects {
		if projects[i].ID == ref {
			return &projects[i], nil
		}
	}
	var hit *cliProject
	for i := range projects {
		if projects[i].Name != ref {
			continue
		}
		if hit != nil {
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("项目名 %q 不唯一，请改用 ID", ref))
		}
		hit = &projects[i]
	}
	if hit == nil {
		return nil, apperr.New(apperr.CodeNotFound, fmt.Sprintf("未找到项目 %q（polaris project list 查看）", ref))
	}
	return hit, nil
}

// resolveChatProject 决定新会话归属：显式 --project > 当前目录匹配 > 默认项目。
// 返回的 ID 为空表示默认项目。项目 API 不可用（旧守护进程 / 远程非本地凭证）时
// 静默退化为默认项目，不阻断对话。
func resolveChatProject(ref string) (*cliProject, error) {
	// 显式指定时连归档项目一起取，才能报"已归档"而不是误导性的"未找到"；
	// 目录匹配本身跳过归档项目（matchProjectByDir）。
	projects, err := cliListProjects(ref != "")
	if err != nil {
		if ref != "" {
			return nil, err
		}
		return nil, nil //nolint:nilerr // 隐式匹配失败不应阻断对话
	}
	if ref != "" {
		p, err := resolveProjectRef(projects, ref)
		if err != nil {
			return nil, err
		}
		if p.Archived {
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("项目 %q 已归档", p.Name))
		}
		return p, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil //nolint:nilerr
	}
	return matchProjectByDir(projects, canonicalDir(cwd)), nil
}

// cliSessionProject 查询会话所属项目；查询失败返回 ok=false（调用方不据此做决定）。
func cliSessionProject(sessionID string) (projectID string, ok bool) {
	var resp struct {
		ProjectID string `json:"project_id"`
	}
	if err := cliRequest("GET", "/v1/sessions/"+url.PathEscape(sessionID)+"?max_chars=1", nil, &resp); err != nil {
		return "", false
	}
	if resp.ProjectID == "" {
		return "", false // 会话行不存在（响应省略该字段）
	}
	return resp.ProjectID, true
}

// ── polaris project ─────────────────────────────────────────────────────────

func runProjectCmd(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printProjectHelp()
		return nil
	}
	if err := cliCheckServer(); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	switch args[0] {
	case "list", "ls":
		return runProjectList(args[1:])
	case "new", "create":
		return runProjectNew(args[1:])
	case "rm", "delete":
		return runProjectRm(args[1:])
	}
	return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("未知子命令: polaris project %s", strings.Join(args, " ")))
}

func printProjectHelp() {
	fmt.Println("用法: polaris project <子命令>")
	fmt.Println()
	fmt.Println("  list [--all]                                   列出项目（* = 当前目录所属项目；--all 含已归档）")
	fmt.Println("  new <名称> [--root <目录> | --here] [--trust]   新建项目；--here 以当前目录为工作目录")
	fmt.Println("      [--instructions <文本>]                    --trust 信任该目录内的 AGENTS.md / CLAUDE.md")
	fmt.Println("  rm <ID|名称>                                   删除项目（其会话迁回默认项目，不删除会话）")
	fmt.Println()
	fmt.Println("polaris chat 在项目目录内运行时，新会话自动归属该项目；也可用 --project <ID|名称> 显式指定。")
	fmt.Println("记忆：对话记忆按项目隔离；用户画像、语义知识与经验总结跨项目共享（ADR-0097 决策三修订）。")
}

func runProjectList(args []string) error {
	all := len(args) > 0 && args[0] == "--all"
	projects, err := cliListProjects(all)
	if err != nil {
		return err
	}
	var cur *cliProject
	if cwd, err := os.Getwd(); err == nil {
		cur = matchProjectByDir(projects, canonicalDir(cwd))
	}
	for _, p := range projects {
		mark := " "
		if cur != nil && cur.ID == p.ID {
			mark = clr(ansiAccent+ansiBold, "*")
		}
		var tags []string
		if p.Trusted {
			tags = append(tags, "trusted")
		}
		if p.Archived {
			tags = append(tags, "archived")
		}
		tag := ""
		if len(tags) > 0 {
			tag = clr(ansiWarn, " ["+strings.Join(tags, ",")+"]")
		}
		root := p.RootPath
		if root == "" {
			root = "-"
		}
		fmt.Printf("%s %-22s %-20s %4d 会话  %s%s\n", mark, clr(ansiDim, p.ID), p.Name, p.SessionCount, clr(ansiDim, root), tag)
	}
	return nil
}

// projectNewArgs polaris project new 的参数。
type projectNewArgs struct {
	name, root, instructions string
	here, trust              bool
}

func parseProjectNewArgs(args []string) (projectNewArgs, error) {
	var o projectNewArgs
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--here":
			o.here = true
		case a == "--trust":
			o.trust = true
		case a == "--root" && i+1 < len(args):
			i++
			o.root = args[i]
		case strings.HasPrefix(a, "--root="):
			o.root = strings.TrimPrefix(a, "--root=")
		case a == "--instructions" && i+1 < len(args):
			i++
			o.instructions = args[i]
		case strings.HasPrefix(a, "-"):
			return o, apperr.New(apperr.CodeInvalidInput, "未知参数: "+a)
		case o.name == "":
			o.name = a
		default:
			return o, apperr.New(apperr.CodeInvalidInput, "多余参数: "+a)
		}
	}
	return o.finalize()
}

// finalize 参数间约束与路径规范化（与逐项扫描分开，各自保持简单）。
func (o projectNewArgs) finalize() (projectNewArgs, error) {
	if o.name == "" {
		return o, apperr.New(apperr.CodeInvalidInput, "用法: polaris project new <名称> [--root <目录> | --here] [--trust]")
	}
	if o.here {
		if o.root != "" {
			return o, apperr.New(apperr.CodeInvalidInput, "--here 与 --root 只能二选一")
		}
		o.root = "."
	}
	// 守护进程要求绝对路径；相对路径按 CLI 进程的 cwd 解析（守护进程的 cwd 与用户无关）。
	if o.root != "" && o.root != "~" && !strings.HasPrefix(o.root, "~/") && !filepath.IsAbs(o.root) {
		abs, err := filepath.Abs(o.root)
		if err != nil {
			return o, apperr.Wrap(apperr.CodeInvalidInput, "解析 --root 失败", err)
		}
		o.root = abs
	}
	if o.trust && o.root == "" {
		return o, apperr.New(apperr.CodeInvalidInput, "--trust 需要同时指定 --root 或 --here")
	}
	return o, nil
}

func runProjectNew(args []string) error {
	o, err := parseProjectNewArgs(args)
	if err != nil {
		return err
	}
	var created cliProject
	if err := cliPost("/v1/projects", map[string]any{
		"name": o.name, "root_path": o.root, "instructions": o.instructions, "trusted": o.trust,
	}, &created); err != nil {
		return err
	}
	fmt.Printf("%s  项目已创建: %s  %s\n", clr(ansiOk, "✓"), created.Name, clr(ansiDim, created.ID))
	if created.RootPath != "" {
		fmt.Printf("   工作目录: %s\n", created.RootPath)
	}
	if created.Trusted {
		fmt.Println(clr(ansiWarn, "   已信任该目录：其中的 AGENTS.md / CLAUDE.md 将作为可信指令进入提示词"))
	}
	return nil
}

func runProjectRm(args []string) error {
	if len(args) != 1 {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris project rm <ID|名称>")
	}
	projects, err := cliListProjects(true)
	if err != nil {
		return err
	}
	p, err := resolveProjectRef(projects, args[0])
	if err != nil {
		return err
	}
	if err := cliRequest("DELETE", "/v1/projects/"+url.PathEscape(p.ID), nil, nil); err != nil {
		return err
	}
	fmt.Printf("%s  已删除项目 %s（%d 个会话已迁回默认项目）\n", clr(ansiOk, "✓"), p.Name, p.SessionCount)
	return nil
}
