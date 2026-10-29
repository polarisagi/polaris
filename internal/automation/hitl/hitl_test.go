package hitl

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	storerepo "github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// newTestGateway 用真实 SQLite（内存库）+ 044 DDL 构造网关，测试与生产走同一条落账路径。
func newTestGateway(t *testing.T) (*GatewayImpl, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ddl, err := schema.FS.ReadFile("044_hitl_requests.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	return NewGateway(storerepo.NewSQLiteHITLRequestRepository(db)), db
}

func TestGatewayImpl_PromptAndRespond(t *testing.T) {
	gw, _ := newTestGateway(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p := types.HITLPrompt{
		ID:             "hitl-123",
		CheckpointType: "test",
		PromptText:     "Approve execution?",
	}

	// 异步响应
	go func() {
		time.Sleep(50 * time.Millisecond)
		err := gw.Respond(context.Background(), p.ID, types.HITLResponse{
			OptionKey: "approve",
		})
		if err != nil {
			t.Errorf("respond failed: %v", err)
		}
	}()

	resp, err := gw.Prompt(ctx, p)
	if err != nil {
		t.Fatalf("prompt failed: %v", err)
	}
	if resp.OptionKey != "approve" {
		t.Fatalf("expected approve, got %s", resp.OptionKey)
	}
}

func TestGatewayImpl_PromptTimeout(t *testing.T) {
	gw, _ := newTestGateway(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := gw.Prompt(ctx, types.HITLPrompt{ID: "hitl-456"})
	if err != context.DeadlineExceeded {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

// TestGatewayImpl_PromptTimeout_DeviceControlFullAccess 验证 inv_M13_07：
// 电脑操控 checkpoint 在 full_access 权限模式下超时应兜底为 auto_approve，
// 与"设置 → 设备操控 → 完全访问(上帝模式)"的产品承诺一致。
func TestGatewayImpl_PromptTimeout_DeviceControlFullAccess(t *testing.T) {
	gw, _ := newTestGateway(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	resp, err := gw.Prompt(ctx, types.HITLPrompt{
		ID:             "hitl-full-access",
		CheckpointType: types.CheckpointDeviceControlReview,
		PermissionMode: types.ModeFullAccess,
	})
	if err != nil {
		t.Fatalf("expected no error (auto_approve), got %v", err)
	}
	if resp == nil || !resp.Approved {
		t.Fatalf("expected auto-approved response, got %+v", resp)
	}
}

// TestGatewayImpl_PromptTimeout_DeviceControlAutoReview 验证 auto_review/default
// 模式下电脑操控 checkpoint 超时不受权限模式影响，维持既有 kill_pause 行为——
// 这两个模式的产品语义是"高危操作需要人审"，超时不应被自动放行。
func TestGatewayImpl_PromptTimeout_DeviceControlAutoReview(t *testing.T) {
	gw, _ := newTestGateway(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := gw.Prompt(ctx, types.HITLPrompt{
		ID:             "hitl-auto-review",
		CheckpointType: types.CheckpointDeviceControlReview,
		PermissionMode: types.ModeAutoReview,
	})
	if err != context.DeadlineExceeded {
		t.Fatalf("expected deadline exceeded (kill_pause), got %v", err)
	}
}

// TestGatewayImpl_PromptTimeout_DeadlineNsIsAbsolute 回归测试：DeadlineNs 曾被
// Prompt() 误当作相对 Duration 又叠加一次 time.Now()，导致所有调用方构造的
// "N 分钟/小时后超时"实际上被推迟到约 56 年后，超时机制形同虚设（2026-07-07
// 修复）。本测试模拟真实调用方写法：不设置外层 ctx 超时（用 context.Background()），
// 完全依赖 DeadlineNs 自己建立截止时间，验证 Prompt() 确实会在 DeadlineNs
// 指定的绝对时间点附近返回，而不是永久阻塞。
func TestGatewayImpl_PromptTimeout_DeadlineNsIsAbsolute(t *testing.T) {
	gw, _ := newTestGateway(t)

	done := make(chan struct{})
	go func() {
		_, _ = gw.Prompt(context.Background(), types.HITLPrompt{
			ID:         "hitl-deadline-ns",
			DeadlineNs: time.Now().Add(50 * time.Millisecond).UnixNano(),
		})
		close(done)
	}()

	select {
	case <-done:
		// 正常：在 DeadlineNs 附近及时返回
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt() did not honor DeadlineNs as an absolute deadline — timeout mechanism is broken")
	}
}

// TestGatewayImpl_PromptTimeout_DeviceControlFullAccessButTainted 验证
// TaintLevel 硬地板优先级高于权限模式：即使 full_access，TaintLevel>=Medium
// 时超时仍必须 auto_deny，防止被污染的 Agent 拿设备操控设置当挡箭牌。
func TestGatewayImpl_PromptTimeout_DeviceControlFullAccessButTainted(t *testing.T) {
	gw, _ := newTestGateway(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	resp, err := gw.Prompt(ctx, types.HITLPrompt{
		ID:             "hitl-full-access-tainted",
		CheckpointType: types.CheckpointDeviceControlReview,
		PermissionMode: types.ModeFullAccess,
		TaintLevel:     types.TaintMedium,
	})
	if err != nil {
		t.Fatalf("expected no error (auto_deny resolves without propagating ctx err), got %v", err)
	}
	if resp == nil || resp.Approved {
		t.Fatalf("expected auto-denied response despite full_access, got %+v", resp)
	}
}
