package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── surrealTx — 伪事务（内存 KV 无真实回滚，MVP 限制）────────────────────────
// FFI 绑定/protocol.Store 基础实现见 surreal_store.go；扩展接口见 surreal_store_ext.go。
// （R7 拆分自 surreal_store.go）

type surrealTx struct{ store *SurrealDBCoreStore }

func (t *surrealTx) Get(key []byte) ([]byte, error) {
	return t.store.Get(context.Background(), key)
}
func (t *surrealTx) Put(key, value []byte) error {
	return t.store.Put(context.Background(), key, value)
}
func (t *surrealTx) Delete(key []byte) error {
	return t.store.Delete(context.Background(), key)
}
func (t *surrealTx) Scan(prefix []byte) (protocol.Iterator, error) {
	return t.store.Scan(context.Background(), prefix)
}

// ─── surrealIterator — 包装 KV scan 结果为 protocol.Iterator ───────────────────

type surrealKVPair struct{ Key, Value []byte }

type surrealIterator struct {
	pairs []surrealKVPair
	pos   int
	err   error
}

func (it *surrealIterator) Next() bool {
	it.pos++
	return it.pos < len(it.pairs)
}
func (it *surrealIterator) Key() []byte   { return it.pairs[it.pos].Key }
func (it *surrealIterator) Value() []byte { return it.pairs[it.pos].Value }
func (it *surrealIterator) Err() error    { return it.err }
func (it *surrealIterator) Close() error  { return nil }

// ─── JSON 解析辅助 ────────────────────────────────────────────────────────────

// kvPairJSON 对应 surreal_kv_scan 返回的 JSON 格式 {"k":"<hex>","v":"<hex>"}
type kvPairJSON struct {
	K string `json:"k"`
	V string `json:"v"`
}

func parseKVPairsJSON(jsonStr string) ([]surrealKVPair, error) {
	var raw []kvPairJSON
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("parseKVPairs: %v", err), err)
	}
	pairs := make([]surrealKVPair, 0, len(raw))
	for _, p := range raw {
		k, err := hex.DecodeString(p.K)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("parseKVPairs key hex: %v", err), err)
		}
		v, err := hex.DecodeString(p.V)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("parseKVPairs val hex: %v", err), err)
		}
		pairs = append(pairs, surrealKVPair{Key: k, Value: v})
	}
	return pairs, nil
}

func parseScoredJSON(jsonStr string) ([]ScoredID, error) {
	var results []ScoredID
	if err := json.Unmarshal([]byte(jsonStr), &results); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("parseScoredJSON: %v", err), err)
	}
	return results, nil
}

func parseIDsJSON(jsonStr string) ([]string, error) {
	var ids []string
	if err := json.Unmarshal([]byte(jsonStr), &ids); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("parseIDsJSON: %v", err), err)
	}
	return ids, nil
}

// SurrealPurge 向 Rust 侧发出内存压缩信号（no-op 安全）。
func SurrealPurge() {
	if surrealPurge != nil {
		surrealPurge()
	}
}
