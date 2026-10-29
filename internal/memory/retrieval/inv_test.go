package retrieval

import (
	"context"
	"fmt"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/memory/store"
)

// Test_inv_M5_01_ImmutableCoreNeverEvicted 验证 ContextWindow.Compress 不驱逐 system 角色消息。
// inv_M5_01: ImmutableCore 永不参与压缩——ContextWindow.Compress 跳过此区域。
func Test_inv_M5_01_ImmutableCoreNeverEvicted(t *testing.T) {
	cw := store.NewContextWindow(5)

	cw.Append(types.Message{Role: "system", Content: "immutable-core-sentinel"})

	for i := 0; i < 20; i++ {
		cw.Append(types.Message{Role: "user", Content: fmt.Sprintf("filler %d", i)})
	}

	if err := cw.Compress(context.Background(), 1); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	msgs := cw.Messages()
	for _, m := range msgs {
		if m.Role == "system" && m.Content == "immutable-core-sentinel" {
			return // pass
		}
	}
	t.Error("inv_M5_01 VIOLATED: ImmutableCore (system role) was evicted during Compress")
}

// Test_inv_M5_01_MultipleSystemMessagesPreserved 验证多条 system 消息均不被驱逐。
func Test_inv_M5_01_MultipleSystemMessagesPreserved(t *testing.T) {
	cw := store.NewContextWindow(3)

	cw.Append(types.Message{Role: "system", Content: "identity-block"})
	cw.Append(types.Message{Role: "system", Content: "volatile-block"})

	for i := 0; i < 10; i++ {
		cw.Append(types.Message{Role: "user", Content: "user message"})
	}

	_ = cw.Compress(context.Background(), 1)

	msgs := cw.Messages()
	systemCount := 0
	for _, m := range msgs {
		if m.Role == "system" {
			systemCount++
		}
	}
	if systemCount != 2 {
		t.Errorf("inv_M5_01 VIOLATED: expected 2 system messages after Compress, got %d", systemCount)
	}
}

// Test_inv_M5_01_OnlySystemRemainsAtZeroBudget 验证极限预算下只剩 system 消息，不报错。
// Compress 应优雅停止（无法继续驱逐），而非死循环或 panic。
func Test_inv_M5_01_OnlySystemRemainsAtZeroBudget(t *testing.T) {
	cw := store.NewContextWindow(10)
	cw.Append(types.Message{Role: "system", Content: "sys"})
	cw.Append(types.Message{Role: "user", Content: "u1"})
	cw.Append(types.Message{Role: "assistant", Content: "a1"})

	if err := cw.Compress(context.Background(), 0); err != nil {
		t.Fatalf("Compress(0): %v", err)
	}

	msgs := cw.Messages()
	for _, m := range msgs {
		if m.Role == "system" {
			return // system 仍在，pass
		}
	}
	t.Error("inv_M5_01 VIOLATED: system message gone at zero-budget Compress")
}
