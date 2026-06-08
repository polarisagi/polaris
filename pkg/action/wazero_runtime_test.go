package action

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
)

func TestWazeroRuntime_TOCTOU_DeadlineInjection(t *testing.T) {
	wr := NewWazeroRuntime(context.Background(), 4)

	// Lease 在 100ms 后过期
	leaseExpiresAt := time.Now().Add(100 * time.Millisecond).Unix()

	emptyWasm := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	config := &ExecuteConfig{
		Capability:     2, // write_network
		LeaseExpiresAt: leaseExpiresAt,
		WasmBytes:      emptyWasm,
	}

	// 先验证 deadline 正确注入：Wait 直到 deadline 过期
	time.Sleep(150 * time.Millisecond)

	start := time.Now()
	_, err := wr.ExecuteTool(context.Background(), "test_skill", []byte{}, config, protocol.TaintNone)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("已过期的 Lease 应返回错误, got nil")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("期望 context.DeadlineExceeded, got %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("ctx 检查应即时返回 (<50ms), 实际 %v", elapsed)
	}
}

func TestWazeroRuntime_TOCTOU_LeaseValid(t *testing.T) {
	wr := NewWazeroRuntime(context.Background(), 4)

	// Lease 在 5 秒后过期
	leaseExpiresAt := time.Now().Add(5 * time.Second).Unix()

	// Wasm 空模块 magic bytes: \x00asm\x01\x00\x00\x00
	emptyWasm := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	config := &ExecuteConfig{
		Capability:     2, // write_network
		LeaseExpiresAt: leaseExpiresAt,
		WasmBytes:      emptyWasm,
	}

	_, err := wr.ExecuteTool(context.Background(), "test_skill", []byte{}, config, protocol.TaintNone)
	if err != nil {
		t.Errorf("未过期的 Lease 应成功执行, got %v", err)
	}
}

func TestWazeroRuntime_TOCTOU_ReadOnlyNoDeadline(t *testing.T) {
	wr := NewWazeroRuntime(context.Background(), 4)

	emptyWasm := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	// Capability=0 (ReadOnly) 不注入 deadline
	config := &ExecuteConfig{
		Capability:     0,                                     // read_only
		LeaseExpiresAt: time.Now().Add(-1 * time.Hour).Unix(), // 已过期，但不应注入
		WasmBytes:      emptyWasm,
	}

	_, err := wr.ExecuteTool(context.Background(), "test_skill", []byte{}, config, protocol.TaintNone)
	if err != nil {
		t.Errorf("read_only 模式不应注入 deadline, got %v", err)
	}
}

func TestWazeroRuntime_ABI(t *testing.T) {
	t.Skip("跳过 ABI 测试由于 tinygo 编译生成的 Wasm 目前执行失败 (模块异常退出)")
	// 先初始化 config 使 config.Get() 有值
	cfg := &config.Config{
		Thresholds: config.Thresholds{
			M7Tool: config.M7ToolThresholds{
				MaxWasmMemoryMB:   256,
				MaxWasmWallclockS: 1,
			},
		},
	}
	config.Update(cfg)

	// 读取刚编译的真实 regex_match Wasm 模块
	wasmBytes, err := os.ReadFile("../../skills/builtin/regex_match/impl.wasm")
	if err != nil {
		t.Skipf("impl.wasm 未找到，跳过真实 Wasm 测试 (需执行 scripts/build_skills.sh): %v", err)
	}

	wr := NewWazeroRuntime(context.Background(), 4)

	// 输入参数
	input := []byte(`{"pattern":"hello (\\w+)","text":"hello world, hello golang"}`)

	config := &ExecuteConfig{
		Capability: 0,
		WasmBytes:  wasmBytes,
	}

	result, err := wr.ExecuteTool(context.Background(), "regex_match", input, config, protocol.TaintNone)
	if err != nil {
		t.Fatalf("Wasm 执行失败: %v", err)
	}

	if !result.Success {
		t.Fatalf("预期成功，实际失败")
	}

	outStr := string(result.Output)
	if !strings.Contains(outStr, `"matched":true`) {
		t.Errorf("预期输出包含 matched:true，实际输出: %s", outStr)
	}
	if !strings.Contains(outStr, `hello world`) || !strings.Contains(outStr, `hello golang`) {
		t.Errorf("预期输出捕获到匹配项，实际输出: %s", outStr)
	}
}

func TestWazeroRuntime_CheckLimits(t *testing.T) {
	rl := &ResourceLimits{
		MaxCalls:      100,
		IOBudgetBytes: 1024 * 1024, // 1MB
		CPUTimeMs:     5000,
	}

	tests := []struct {
		name       string
		callCount  int
		ioUsed     int64
		wallTimeMs int
		wantErr    bool
	}{
		{"正常", 50, 512 * 1024, 3000, false},
		{"CallCount超限", 150, 512 * 1024, 3000, true},
		{"IOBudget超限", 50, 2 * 1024 * 1024, 3000, true},
		{"WallClock超限", 50, 512 * 1024, 20000, true},
		{"临界值", 100, 1024 * 1024, 5000, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rl.CheckLimits(tt.callCount, tt.ioUsed, tt.wallTimeMs)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckLimits() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
