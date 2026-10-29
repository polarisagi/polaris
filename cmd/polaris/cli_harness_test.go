package main

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestCLIHarness_InvalidArgs(t *testing.T) {
	// 无子命令
	if err := runHarnessCmd(nil); err == nil || apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for empty args, got %v", err)
	}

	// 未知子命令
	if err := runHarnessCmd([]string{"unknown"}); err == nil || apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for unknown subcommand, got %v", err)
	}

	// rollback 缺少版本
	if err := runHarnessRollback(nil); err == nil || apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for empty rollback, got %v", err)
	}

	// rollback 未知选项
	if err := runHarnessRollback([]string{"--bad-opt"}); err == nil || apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for invalid option, got %v", err)
	}

	// history 负数 limit
	if err := runHarnessHistory([]string{"--limit=-1"}); err == nil || apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for negative limit, got %v", err)
	}
}
