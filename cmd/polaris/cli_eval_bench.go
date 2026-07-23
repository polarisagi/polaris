package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/polarisagi/polaris/internal/eval/harness/benchmark"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func runEvalBenchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	suite := fs.String("suite", "", "基准套件名称 (例如 tau-bench)")
	dataPath := fs.String("data", "", "本地数据集路径")
	outPath := fs.String("out", "", "可选：输出报告 JSON 路径")

	if err := fs.Parse(args); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "parse bench flags failed", err)
	}
	if *suite == "" || *dataPath == "" {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris eval bench --suite=<suite> --data=<path> [--out=<report.json>]")
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

	// ADR-0068 决策原文："评测的实际执行仍然统一走现有的 RunnerImpl 机制"——
	// 但 RunnerImpl.RunSuite 依赖完整的 protocol.Store / SQLiteEvalStore / Agent
	// 运行环境，与本命令期望的"离线、无需启动完整 Agent 环境"用法不兼容，接线
	// 属于独立工作量，本轮不实现。
	//
	// 此前版本在此处对每条用例无条件计数为 pass 并写入报告——这是伪造的验证结果
	// （违反 HE-2 可验证执行：不允许编造未发生的执行）。宁可诚实报告"未执行"，
	// 也不能让报告 JSON 看起来像是真实跑过 Agent 的 100% 通过率。
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
