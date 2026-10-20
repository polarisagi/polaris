package repo

import (
	"context"
	"testing"

	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestChatTurnRepo_StepsRoundTripCascadeAndPurge(t *testing.T) {
	db := newChatAppViewsTestDB(t)
	chat := NewSQLiteChatRepository(db)
	turns := NewSQLiteChatTurnRepository(db, db)
	ctx := context.Background()
	if err := chat.CreateSession(ctx, types.ChatSessionRow{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	mid, err := chat.AppendMessage(ctx, types.ChatMessageRow{SessionID: "s1", Role: "assistant", Content: "x"})
	if err != nil {
		t.Fatal(err)
	}

	rows := []protorepo.TurnStepRow{
		{TurnID: "t1", SessionID: "s1", Seq: 2, Kind: "tool", Data: []byte(`{"id":"b"}`), CreatedAtMs: 100},
		{TurnID: "t1", SessionID: "s1", Seq: 0, Kind: "summary", Data: []byte(`{"steps":2}`), CreatedAtMs: 200},
		{TurnID: "t1", SessionID: "s1", Seq: 1, Kind: "thinking", Data: []byte(`{"id":"a"}`), CreatedAtMs: 100},
	}
	if err := turns.AppendTurnSteps(ctx, rows); err != nil {
		t.Fatal(err)
	}
	// 重复写入同 (turn,seq) 幂等。
	if err := turns.AppendTurnSteps(ctx, rows[:1]); err != nil {
		t.Fatal(err)
	}
	got, err := turns.ListTurnSteps(ctx, "s1", "t1")
	if err != nil || len(got) != 3 || got[0].Seq != 0 || got[2].Seq != 2 {
		t.Fatalf("list = %+v err=%v", got, err)
	}
	if other, _ := turns.ListTurnSteps(ctx, "s2", "t1"); len(other) != 0 {
		t.Errorf("跨会话不应读到步骤: %+v", other)
	}
	sums, err := turns.TurnSummaries(ctx, "s1")
	if err != nil || string(sums["t1"]) != `{"steps":2}` {
		t.Fatalf("summaries = %v err=%v", sums, err)
	}

	if err := turns.AttachTurnMeta(ctx, mid, "t1", `{"in":1}`); err != nil {
		t.Fatal(err)
	}
	msgs, err := chat.ListMessages(ctx, "s1", 10)
	if err != nil || len(msgs) != 1 || msgs[0].TurnID != "t1" || msgs[0].Usage != `{"in":1}` {
		t.Fatalf("messages = %+v err=%v", msgs, err)
	}

	n, err := turns.PurgeTurnStepsBefore(ctx, 150)
	if err != nil || n != 2 {
		t.Fatalf("purge n=%d err=%v, want 2", n, err)
	}

	// 测试库只建了 013_chat.sql（无 session_todos），直接删会话行验证 FK 级联。
	if _, err := db.Exec(`DELETE FROM chat_sessions WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chat_turn_steps`).Scan(&left); err != nil || left != 0 {
		t.Errorf("删除会话后应级联清空步骤, left=%d err=%v", left, err)
	}
}
