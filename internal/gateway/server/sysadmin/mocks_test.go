package sysadmin

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// mockPrefsRepo/mockPolicyGate 2026-07-07 从 mcp_servers_extra_test.go 抽出：
// 该文件随 mcp_servers.go 一起拆到独立的 mcpadmin 子包，但这两个 mock 类型
// 还被 doctor_export_test.go/tools_handlers_extra_test.go/tools_handlers_test.go
// 等仍留在 sysadmin 包内的测试文件共用，故在此保留一份供 sysadmin 包内测试使用
// （mcpadmin 包内测试用的是各自文件里的独立副本，Go 测试文件类型不能跨包共享）。
type mockPrefsRepo struct{}

func (m mockPrefsRepo) GetPermissionMode(ctx context.Context) (types.PermissionMode, error) {
	return types.ModeAutoReview, nil
}
func (m mockPrefsRepo) SetPermissionMode(ctx context.Context, mode types.PermissionMode) error {
	return nil
}
func (m mockPrefsRepo) GetMaxBudget(ctx context.Context) (float64, error)           { return 0, nil }
func (m mockPrefsRepo) SetMaxBudget(ctx context.Context, limit float64) error       { return nil }
func (m mockPrefsRepo) GetActiveLLMProvider(ctx context.Context) (string, error)    { return "", nil }
func (m mockPrefsRepo) SetActiveLLMProvider(ctx context.Context, prov string) error { return nil }
func (m mockPrefsRepo) GetSystemPromptVersion(ctx context.Context) (int, error)     { return 0, nil }

type mockPolicyGate struct{}

func (m mockPolicyGate) Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}
func (m mockPolicyGate) AnalyzeHook(ctx context.Context, script string) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}
func (m mockPolicyGate) TaintData(ctx context.Context, data []byte, source string, trustLevel int) ([]byte, error) {
	return data, nil
}
func (m mockPolicyGate) IsAuthorized(ctx context.Context, principal string, action string, resource string, reqCtx map[string]any) (bool, error) {
	return true, nil
}
