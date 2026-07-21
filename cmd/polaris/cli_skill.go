// polaris skill 子命令组：用户意图驱动的技能生成（2026-07-21 deadcode 审查
// 补齐，ADR-0052）。与 polaris eval / polaris allowlist 一样，是纯 HTTP 客户端
// 薄封装——真正的生成逻辑（LLM 调用 + 落盘 + 安装 + 注册）跑在 polaris serve
// 侧（internal/gateway/server/sysadmin.HandleCreateSkill），CLI 只负责把用户的
// 自然语言描述发过去。
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func runSkillCmd(args []string) error {
	if len(args) == 0 {
		printSkillHelp()
		return nil
	}
	switch args[0] {
	case "create":
		return runSkillCreate(args[1:])
	case "help", "-h", "--help":
		printSkillHelp()
		return nil
	}
	return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("未知子命令: polaris skill %s", strings.Join(args, " ")))
}

func printSkillHelp() {
	fmt.Println("用法: polaris skill <子命令>")
	fmt.Println()
	fmt.Println(`  create --intent "<工作流描述>"   用自然语言描述生成一个新技能（LLM 生成 SKILL.md 并安装）`)
}

func runSkillCreate(args []string) error {
	var intent string
	for i, a := range args {
		if (a == "--intent" || a == "-i") && i+1 < len(args) {
			intent = args[i+1]
		}
	}
	if intent == "" {
		return apperr.New(apperr.CodeInvalidInput, `用法: polaris skill create --intent "<工作流描述>"`)
	}
	if err := cliCheckServer(); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	var result map[string]any
	if err := cliPost("/v1/skills/create", map[string]any{"intent": intent}, &result); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	fmt.Printf("%s  技能已生成: %v\n", clr(ansiOk, "✓"), result["plugin_dir"])
	return nil
}
