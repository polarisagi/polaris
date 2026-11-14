package lifecycle

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestChannelService_MessageIDDeduplication(t *testing.T) {
	svc, fm, _ := installChannelPlugin(t)
	turns := &recordingTurns{done: make(chan struct{}, 8)}
	svc.BindTurns(turns)
	enableTestChannel(t, svc)

	// 1. 发送 msg_001 -> 派发成功
	meta1 := map[string]string{"chat_id": "c1", "user_id": "u1", "message_id": "msg_001"}
	params1, _ := json.Marshal(map[string]any{"content": "first", "meta": meta1})
	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodChannelEvent, params1)
	select {
	case <-turns.done:
	case <-time.After(2 * time.Second):
		t.Fatal("first message was not dispatched")
	}

	// 2. 发送重复 msg_001 -> 丢弃
	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodChannelEvent, params1)
	select {
	case <-turns.done:
		t.Fatal("duplicate message must not be dispatched")
	case <-time.After(150 * time.Millisecond):
	}

	// 3. 发送不同 msg_002 -> 派发成功
	meta2 := map[string]string{"chat_id": "c1", "user_id": "u1", "message_id": "msg_002"}
	params2, _ := json.Marshal(map[string]any{"content": "second", "meta": meta2})
	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodChannelEvent, params2)
	select {
	case <-turns.done:
	case <-time.After(2 * time.Second):
		t.Fatal("second message was not dispatched")
	}

	// 4. 空串 message_id 在 key 存在时必须报错丢弃
	metaEmpty := map[string]string{"chat_id": "c1", "user_id": "u1", "message_id": "   "}
	paramsEmpty, _ := json.Marshal(map[string]any{"content": "empty", "meta": metaEmpty})
	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodChannelEvent, paramsEmpty)
	select {
	case <-turns.done:
		t.Fatal("empty message_id must be rejected")
	case <-time.After(150 * time.Millisecond):
	}

	// 5. 非法字符 message_id 必须报错丢弃
	metaBad := map[string]string{"chat_id": "c1", "user_id": "u1", "message_id": "bad\x00id"}
	paramsBad, _ := json.Marshal(map[string]any{"content": "bad", "meta": metaBad})
	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodChannelEvent, paramsBad)
	select {
	case <-turns.done:
		t.Fatal("control-char message_id must be rejected")
	case <-time.After(150 * time.Millisecond):
	}

	// 6. 超过 256 字节 message_id 必须报错丢弃
	metaTooLong := map[string]string{"chat_id": "c1", "user_id": "u1", "message_id": strings.Repeat("m", 257)}
	paramsTooLong, _ := json.Marshal(map[string]any{"content": "long", "meta": metaTooLong})
	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodChannelEvent, paramsTooLong)
	select {
	case <-turns.done:
		t.Fatal("too-long message_id must be rejected")
	case <-time.After(150 * time.Millisecond):
	}
}

type blockingTurnRunner struct {
	started chan struct{}
	ctxDone chan struct{}
}

func (r *blockingTurnRunner) RunChannelTurn(ctx context.Context, _, _ string) error {
	r.started <- struct{}{}
	<-ctx.Done()
	r.ctxDone <- struct{}{}
	return ctx.Err()
}

func TestChannelService_DisableCancelsInFlightTurn(t *testing.T) {
	svc, fm, _ := installChannelPlugin(t)
	runner := &blockingTurnRunner{
		started: make(chan struct{}, 1),
		ctxDone: make(chan struct{}, 1),
	}
	svc.BindTurns(runner)
	enableTestChannel(t, svc)

	params := json.RawMessage(`{"content":"block-me","meta":{"chat_id":"c1","user_id":"u1"}}`)
	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodChannelEvent, params)

	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}

	// 在途停用 channel
	if err := svc.SetBindingState(context.Background(), types.PluginChannelState{
		PluginID: "pl_t",
		Server:   "tg",
		Enabled:  false,
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-runner.ctxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight turn context was not cancelled when channel was disabled")
	}
}

func TestChannelService_QueuedEventDroppedOnDisableAndReenable(t *testing.T) {
	svc, fm, extRepo := installChannelPlugin(t)
	turns := &recordingTurns{done: make(chan struct{}, 1)}
	svc.BindTurns(turns)
	enableTestChannel(t, svc)

	repo := &signalingChannelRepo{ExtensionRepository: extRepo, statesRead: make(chan struct{}, 4)}
	svc.extRepo = repo

	// 先获取目标 session 锁，使事件在初检通过后卡在拿锁前
	queuedSession := routedChannelSessionID("pl_t", "tg", channelRoute{chatID: "room", userID: "alice"})
	lock := svc.sessionLock(queuedSession)
	lock.Lock()

	params := json.RawMessage(`{"content":"queued","meta":{"chat_id":"room","user_id":"alice"}}`)
	svc.HandleNotification("plugin_pl_t_tg", fm.meta["plugin_pl_t_tg"], methodChannelEvent, params)

	select {
	case <-repo.statesRead:
	case <-time.After(2 * time.Second):
		lock.Unlock()
		t.Fatal("notification did not perform initial binding check")
	}

	// 停用通道（递增 generation 并取消旧世代）
	if err := svc.SetBindingState(context.Background(), types.PluginChannelState{
		PluginID: "pl_t",
		Server:   "tg",
		Enabled:  false,
	}); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}

	// 重新启用通道（新世代），此时如果未做 generation gating，旧事件出锁后会误判 active
	if err := svc.SetBindingState(context.Background(), types.PluginChannelState{
		PluginID: "pl_t",
		Server:   "tg",
		Enabled:  true,
	}); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}

	lock.Unlock()

	select {
	case <-turns.done:
		t.Fatal("queued event from old generation must be dropped even if channel is re-enabled")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestChannelService_ConflictCheckerBlocksEnable(t *testing.T) {
	svc, _, _ := installChannelPlugin(t)
	ctx := context.Background()

	svc.SetConflictChecker(func(ctx context.Context, pluginID, pluginName, server string) error {
		if server == "tg" {
			return apperr.New(apperr.CodeConflict, "内置 telegram 通道已启用，请先停用内置通道再启用插件通道")
		}
		return nil
	})

	err := svc.SetBindingState(ctx, types.PluginChannelState{
		PluginID: "pl_t",
		Server:   "tg",
		Enabled:  true,
	})
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("expected CodeConflict, got %s: %v", apperr.CodeOf(err), err)
	}

	// 确认绑定状态仍为未启用
	bindings, err := svc.ListBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Enabled {
		t.Fatalf("binding must remain disabled: %+v", bindings)
	}
}
