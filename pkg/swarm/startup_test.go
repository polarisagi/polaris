package swarm

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

type mockPinger struct {
	err error
}

func (m *mockPinger) Ping(ctx context.Context) error {
	return m.err
}

func TestPhasedStartup_AllPhasePass(t *testing.T) {
	phases := []PhaseEntry{
		{
			Phase:   PhasePolicy,
			Name:    "Policy",
			Pingers: []Pinger{&mockPinger{err: nil}},
			OnFail: func(p Phase, err error) {
				t.Fatalf("unexpected fail")
			},
		},
	}
	startup := NewPhasedStartup(phases)
	err := startup.Run(context.Background())
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestPhasedStartup_P0Fail(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("The code did not panic")
		}
	}()

	phases := []PhaseEntry{
		{
			Phase:   PhasePolicy,
			Name:    "Policy",
			Pingers: []Pinger{&mockPinger{err: errors.New("timeout")}},
			OnFail: func(p Phase, err error) {
				panic(err)
			},
		},
	}
	startup := NewPhasedStartup(phases)
	_ = startup.Run(context.Background())
}

func TestPhasedStartup_P1FailDoesNotPanic(t *testing.T) {
	phases := []PhaseEntry{
		{
			Phase:   PhaseMemory,
			Name:    "Memory",
			Pingers: []Pinger{&mockPinger{err: errors.New("timeout")}},
			OnFail: func(p Phase, err error) {
				// do not panic
			},
		},
	}
	startup := NewPhasedStartup(phases)
	err := startup.Run(context.Background())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestSQLiteBlackboard_Ping(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer db.Close()
	bb := NewSQLiteBlackboard(db)
	err = bb.Ping(context.Background())
	if err != nil {
		t.Fatalf("expected nil ping, got %v", err)
	}
}
