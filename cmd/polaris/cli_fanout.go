package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func runCSVFanoutCmd(args []string) error {
	if err := cliCheckServer(); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}

	fs := flag.NewFlagSet("csv-fanout", flag.ExitOnError)
	csvPath := fs.String("csv", "", "Path to the input CSV file")
	idColumn := fs.String("id-col", "", "Column name to use as ID (optional)")
	instruction := fs.String("instruction", "", "Instruction template")
	outputPath := fs.String("out", "", "Path to the output CSV file")
	maxConcurrency := fs.Int("concurrency", 0, "Max concurrency")
	maxRuntimeSec := fs.Int("runtime", 1800, "Max runtime in seconds")

	if err := fs.Parse(args); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "parse flags", err)
	}

	if *csvPath == "" || *instruction == "" {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ --csv and --instruction are required"))
		fs.Usage()
		os.Exit(1)
	}

	job := orchestrator.CSVFanoutJob{
		CSVPath:        *csvPath,
		IDColumn:       *idColumn,
		Instruction:    *instruction,
		OutputCSVPath:  *outputPath,
		MaxConcurrency: *maxConcurrency,
		MaxRuntimeSec:  *maxRuntimeSec,
	}

	fmt.Println(clr(ansiAccent, "🚀 Submitting CSV Fanout job to server..."))

	var res orchestrator.FanoutResult
	if err := cliPost("/v1/admin/tasks/csv-fanout", job, &res); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ Submit failed: "+err.Error()))
		return err
	}

	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(clr(ansiOk, "✓ Job completed:"))
	fmt.Println(string(out))

	return nil
}
