package store

import (
	"context"
	"os"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestSQLiteStore_BasicOps(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "test_*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	s, err := OpenSQLite(f.Name(), nil)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// Get 不存在的 key → ErrNotFound
	_, err = s.Get(ctx, []byte("missing"))
	if err != apperr.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Put + Get
	if err := s.Put(ctx, []byte("hello"), []byte("world")); err != nil {
		t.Fatal(err)
	}
	val, err := s.Get(ctx, []byte("hello"))
	if err != nil || string(val) != "world" {
		t.Fatalf("Get after Put: err=%v val=%s", err, val)
	}

	// Delete + Get → ErrNotFound
	if err := s.Delete(ctx, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, err = s.Get(ctx, []byte("hello"))
	if err != apperr.ErrNotFound {
		t.Fatalf("expected ErrNotFound after Delete, got %v", err)
	}
}

func TestSQLiteStore_BatchWrite(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "test_*.db")
	f.Close()
	s, _ := OpenSQLite(f.Name(), nil)
	defer s.Close()
	ctx := context.Background()

	ops := []types.Op{
		{Key: []byte("a"), Value: []byte("1"), Type: types.OpPut},
		{Key: []byte("b"), Value: []byte("2"), Type: types.OpPut},
		{Key: []byte("c"), Value: []byte("3"), Type: types.OpPut},
	}
	if err := s.BatchWrite(ctx, ops); err != nil {
		t.Fatal(err)
	}

	for _, op := range ops {
		v, err := s.Get(ctx, op.Key)
		if err != nil {
			t.Fatalf("Get %s: %v", op.Key, err)
		}
		if string(v) != string(op.Value) {
			t.Fatalf("mismatch: got %s want %s", v, op.Value)
		}
	}
}

func TestSQLiteStore_Scan(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "test_*.db")
	f.Close()
	s, _ := OpenSQLite(f.Name(), nil)
	defer s.Close()
	ctx := context.Background()

	for _, pair := range [][2]string{
		{"prefix/a", "1"}, {"prefix/b", "2"}, {"other/c", "3"},
	} {
		s.Put(ctx, []byte(pair[0]), []byte(pair[1]))
	}

	iter, err := s.Scan(ctx, []byte("prefix/"))
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()

	var keys []string
	for iter.Next() {
		keys = append(keys, string(iter.Key()))
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d: %v", len(keys), keys)
	}
}

func TestSQLiteStore_Txn(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "test_*.db")
	f.Close()
	s, _ := OpenSQLite(f.Name(), nil)
	defer s.Close()
	ctx := context.Background()

	// 正常事务：提交
	err := s.Txn(ctx, func(tx protocol.Transaction) error {
		return tx.Put([]byte("txn_key"), []byte("txn_val"))
	})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.Get(ctx, []byte("txn_key"))
	if string(v) != "txn_val" {
		t.Fatalf("expected txn_val, got %s", v)
	}

	// 失败事务：回滚，txn_key2 不应写入
	s.Txn(ctx, func(tx protocol.Transaction) error {
		tx.Put([]byte("txn_key2"), []byte("should_rollback"))
		return apperr.ErrNotFound // 任意非 nil 错误
	})
	_, err = s.Get(ctx, []byte("txn_key2"))
	if err != apperr.ErrNotFound {
		t.Fatal("rolled-back key should not exist")
	}
}

func TestSQLiteStore_Capabilities(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "test_*.db")
	f.Close()
	s, _ := OpenSQLite(f.Name(), nil)
	defer s.Close()

	caps := s.Capabilities()
	if !caps.SupportsSQL {
		t.Fatal("expected SupportsSQL=true")
	}
	if caps.Engine != "sqlite-wal" {
		t.Fatalf("expected engine sqlite-wal, got %s", caps.Engine)
	}
}
