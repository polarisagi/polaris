// polaris harness 子命令组（ADR-0115 WP6，ADR-0096 决策一）。
// 纯 HTTP 客户端：通过 /v1/harness 端点与守护进程通信。
package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func runHarnessCmd(args []string) error {
	if len(args) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris harness <history|rollback> [选项]")
	}

	switch args[0] {
	case "history", "log":
		return runHarnessHistory(args[1:])
	case "rollback":
		return runHarnessRollback(args[1:])
	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("harness: 未知子命令 %q (可选: history, rollback)", args[0]))
	}
}

func runHarnessHistory(args []string) error {
	fs := flag.NewFlagSet("harness history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 20, "最多列出版本数")
	if err := fs.Parse(args); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "参数解析失败", err)
	}
	if *limit <= 0 {
		return apperr.New(apperr.CodeInvalidInput, "--limit 必须为正整数")
	}

	path := fmt.Sprintf("/v1/harness/history?limit=%d", *limit)
	var resp struct {
		Versions []struct {
			Hash      string `json:"hash"`
			Author    string `json:"author"`
			Timestamp string `json:"timestamp"`
			Message   string `json:"message"`
		} `json:"versions"`
		Count int `json:"count"`
	}
	if err := cliRequest("GET", path, nil, &resp); err != nil {
		return err
	}

	if len(resp.Versions) == 0 {
		fmt.Println("Harness 覆盖层无版本提交记录。")
		return nil
	}

	fmt.Printf("Harness 覆盖层版本历史 (共 %d 条):\n", resp.Count)
	for _, v := range resp.Versions {
		hashShort := v.Hash
		if len(hashShort) > 8 {
			hashShort = hashShort[:8]
		}
		fmt.Printf("  %s  %s  %s\n", clr(ansiDim, hashShort), clr(ansiOk, v.Timestamp), v.Message)
	}
	return nil
}

func runHarnessRollback(args []string) error {
	if len(args) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris harness rollback <version>")
	}
	version := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return apperr.New(apperr.CodeInvalidInput, "未知选项: "+a)
		}
		if version == "" {
			version = a
		} else {
			return apperr.New(apperr.CodeInvalidInput, "多余参数: "+a)
		}
	}
	if version == "" {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris harness rollback <version>")
	}

	req := map[string]any{
		"version": version,
	}
	var resp struct {
		Success    bool   `json:"success"`
		RestoredTo string `json:"restored_to"`
	}
	if err := cliRequest("POST", "/v1/harness/rollback", req, &resp); err != nil {
		return err
	}

	fmt.Printf("%s  Harness 覆盖层已成功回滚到版本: %s\n", clr(ansiOk, "✓"), version)
	return nil
}
