// polaris checkpoint 子命令组（ADR-0115 WP1，ADR-0096 决策一）。
// 纯 HTTP 客户端：通过 /v1/checkpoints 端点与守护进程通信。
package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func runCheckpointCmd(args []string) error {
	if len(args) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris checkpoint <list|restore> [选项]")
	}

	switch args[0] {
	case "list", "ls":
		return runCheckpointList(args[1:])
	case "restore":
		return runCheckpointRestore(args[1:])
	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("checkpoint: 未知子命令 %q (可选: list, restore)", args[0]))
	}
}

type checkpointListArgs struct {
	projectDir string
	limit      int
}

func parseCheckpointListArgs(args []string) (checkpointListArgs, error) {
	fs := flag.NewFlagSet("checkpoint list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 20, "最多列出快照数")
	project := fs.String("project", "", "项目根目录")
	if err := fs.Parse(args); err != nil {
		return checkpointListArgs{}, apperr.Wrap(apperr.CodeInvalidInput, "参数解析失败", err)
	}
	if *limit <= 0 {
		return checkpointListArgs{}, apperr.New(apperr.CodeInvalidInput, "--limit 必须为正整数")
	}
	proj := *project
	if proj == "" && fs.NArg() > 0 {
		proj = fs.Arg(0)
	}
	if proj == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return checkpointListArgs{}, apperr.Wrap(apperr.CodeInternal, "获取当前工作目录失败", err)
		}
		proj = cwd
	}
	abs, err := filepath.Abs(proj)
	if err != nil {
		return checkpointListArgs{}, apperr.Wrap(apperr.CodeInvalidInput, "解析项目目录失败", err)
	}
	return checkpointListArgs{projectDir: abs, limit: *limit}, nil
}

func runCheckpointList(args []string) error {
	o, err := parseCheckpointListArgs(args)
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/v1/checkpoints?project_dir=%s&limit=%d", url.QueryEscape(o.projectDir), o.limit)
	var resp struct {
		ProjectDir string `json:"project_dir"`
		Snapshots  []struct {
			Hash      string `json:"hash"`
			Message   string `json:"message"`
			Timestamp string `json:"timestamp"`
		} `json:"snapshots"`
		Count int `json:"count"`
	}
	if err := cliRequest("GET", path, nil, &resp); err != nil {
		return err
	}

	if len(resp.Snapshots) == 0 {
		fmt.Printf("项目目录 %s 无历史快照记录。\n", o.projectDir)
		return nil
	}

	fmt.Printf("项目快照列表 (%s, 共 %d 条):\n", o.projectDir, resp.Count)
	for _, s := range resp.Snapshots {
		hashShort := s.Hash
		if len(hashShort) > 8 {
			hashShort = hashShort[:8]
		}
		fmt.Printf("  %s  %s  %s\n", clr(ansiDim, hashShort), clr(ansiOk, s.Timestamp), s.Message)
	}
	return nil
}

func runCheckpointRestore(args []string) error {
	if len(args) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris checkpoint restore <hash> [--project <目录>]")
	}
	hash := ""
	projectDir := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--project" && i+1 < len(args):
			i++
			projectDir = args[i]
		case strings.HasPrefix(a, "--project="):
			projectDir = strings.TrimPrefix(a, "--project=")
		case strings.HasPrefix(a, "-"):
			return apperr.New(apperr.CodeInvalidInput, "未知参数: "+a)
		default:
			if hash == "" {
				hash = a
			} else {
				return apperr.New(apperr.CodeInvalidInput, "多余参数: "+a)
			}
		}
	}
	if hash == "" {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris checkpoint restore <hash> [--project <目录>]")
	}
	if projectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "获取当前工作目录失败", err)
		}
		projectDir = cwd
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "解析项目目录失败", err)
	}

	var resp struct {
		RestoredTo string `json:"restored_to"`
		ProjectDir string `json:"project_dir"`
		Message    string `json:"message"`
	}
	req := map[string]any{
		"project_dir": abs,
		"hash":        hash,
	}
	if err := cliRequest("POST", "/v1/checkpoints/restore", req, &resp); err != nil {
		return err
	}
	fmt.Printf("%s  项目工作区已成功恢复到快照: %s\n", clr(ansiOk, "✓"), hash)
	return nil
}
