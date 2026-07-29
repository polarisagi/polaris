package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/internal/eval/harness/benchmark"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func runEvalBenchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	suite := fs.String("suite", "", "基准套件名称 (例如 tau-bench)")
	dataPath := fs.String("data", "", "本地数据集路径")
	outPath := fs.String("out", "", "可选：输出报告 JSON 路径")
	execute := fs.Bool("execute", false, "是否真实运行评测")

	if err := fs.Parse(args); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "parse bench flags failed", err)
	}
	if *suite == "" || *dataPath == "" {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris eval bench --suite=<suite> --data=<path> [--execute] [--out=<report.json>]")
	}

	adapter := benchmark.GetAdapter(*suite)
	if adapter == nil {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("未知的基准套件: %s", *suite))
	}

	ctx := context.Background()
	cases, err := adapter.Load(ctx, *dataPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "load dataset failed", err)
	}

	fmt.Printf("加载了 %d 条来自 %s 的测试用例\n", len(cases), *suite)

	if *execute {
		// This should not happen because main.go intercepts --execute
		return apperr.New(apperr.CodeInternal, "--execute mode must be intercepted by main.go")
	}

	report := map[string]any{
		"suite":    *suite,
		"total":    len(cases),
		"executed": false,
		"note":     "本命令目前仅验证数据集加载/转换，尚未接入 RunnerImpl 实际执行；不产出 pass/fail 结果",
	}

	if *outPath != "" {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "failed to marshal report", err)
		}
		if err := os.WriteFile(*outPath, b, 0644); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "failed to write report", err)
		}
		fmt.Printf("报告已写入 %s（仅数据集加载校验，未执行评测）\n", *outPath)
	} else {
		fmt.Printf("数据集加载校验完成: 总计 %d 条用例；执行环节尚未接入 RunnerImpl\n", len(cases))
	}

	return nil
}

func runEvalBenchCmdWithDeps(args []string, store *harness.SQLiteEvalStore, runner *harness.RunnerImpl) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	suite := fs.String("suite", "", "基准套件名称 (例如 tau-bench)")
	dataPath := fs.String("data", "", "本地数据集路径")
	outPath := fs.String("out", "", "可选：输出报告 JSON 路径")
	execute := fs.Bool("execute", false, "是否真实运行评测")

	if err := fs.Parse(args); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "parse bench flags failed", err)
	}
	if *suite == "" || *dataPath == "" || !*execute {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris eval bench --suite=<suite> --data=<path> --execute [--out=<report.json>]")
	}

	adapter := benchmark.GetAdapter(*suite)
	if adapter == nil {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("未知的基准套件: %s", *suite))
	}

	ctx := context.Background()
	cases, err := adapter.Load(ctx, *dataPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "load dataset failed", err)
	}

	fmt.Printf("加载了 %d 条来自 %s 的测试用例\n", len(cases), *suite)

	// Write cases into the store
	for _, c := range cases {
		// Use "auto_gen" or something for author
		if err := store.PutCase(ctx, *suite, "benchmark_loader", c); err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("failed to store case %s", c.ID), err)
		}
	}
	fmt.Printf("已将 %d 条测试用例存入数据库\n", len(cases))

	// Run the suite
	fmt.Printf("开始执行评测...\n")
	report, err := runner.RunSuite(ctx, *suite, "benchmark")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "runner failed", err)
	}

	if *outPath != "" {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "failed to marshal report", err)
		}
		if err := os.WriteFile(*outPath, b, 0644); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "failed to write report", err)
		}
		fmt.Printf("完整报告已写入 %s\n", *outPath)
	}

	if report.Status == "failed" {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("eval benchmark failed: pass=%d fail=%d", report.PassCount, report.FailCount))
	}

	fmt.Printf("评测执行完成: pass=%d fail=%d\n", report.PassCount, report.FailCount)
	return nil
}
