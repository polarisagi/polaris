// Package storage — SurrealDBCoreStore purego 桥接。
// 历史: 原 cgo 实现已按 ADR-0011 Phase 3 迁移到 purego。
// 架构文档: docs/arch/M02-Storage-Fabric.md §10，ADR-0010
//
// SurrealDBCoreStore — [Storage-SurrealDB-Core] 认知检索轴的 Go 封装。
// 实现 protocol.Store + 扩展接口（VectorStore / GraphStore / FTSStore）。
//
// 后端选择: surreal-mem（默认，kv-mem）/ surreal-rocksdb（显式，≥16GB）。
// kv-mem 进程重启后数据丢失；持久化由 SQLite Outbox 投影负责（M02 §2.5）。
package storage

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	sffi "github.com/polarisagi/polaris/pkg/substrate/ffi"
)

// ─── purego FFI 函数指针（懒绑定，sync.Once 幂等）─────────────────────────────

var (
	surrealOnce sync.Once
	surrealErr  error

	surrealOpen          func(backend string, dbPath string, vecDim int32) int32
	surrealKvGet         func(key *byte, keyLen uintptr, outVal *uintptr, outLen *uintptr) int32
	surrealKvPut         func(key *byte, keyLen uintptr, val *byte, valLen uintptr) int32
	surrealKvDelete      func(key *byte, keyLen uintptr) int32
	surrealKvScan        func(prefix *byte, prefixLen uintptr, outJSON *uintptr) int32
	surrealVecUpsert     func(id string, embed *float32, dim uintptr) int32
	surrealVecKnn        func(query *float32, dim uintptr, k uintptr, outJSON *uintptr) int32
	surrealVecSetMode    func(mode int32) int32
	surrealGraphRelate   func(fromID, edgeType, toID string) int32
	surrealGraphTraverse func(startID, edgeType string, maxDepth uintptr, outJSON *uintptr) int32
	surrealFTSIndex      func(docID, text string) int32
	surrealFTSSearch     func(query string, k uintptr, outJSON *uintptr) int32
	surrealFreeString    func(ptr uintptr)
	surrealFreeBuf       func(ptr uintptr, length uintptr)
)

func bindSurreal() error {
	surrealOnce.Do(func() {
		lib, err := sffi.Load()
		if err != nil {
			surrealErr = err
			return
		}
		purego.RegisterLibFunc(&surrealOpen, lib, "surreal_open")
		purego.RegisterLibFunc(&surrealKvGet, lib, "surreal_kv_get")
		purego.RegisterLibFunc(&surrealKvPut, lib, "surreal_kv_put")
		purego.RegisterLibFunc(&surrealKvDelete, lib, "surreal_kv_delete")
		purego.RegisterLibFunc(&surrealKvScan, lib, "surreal_kv_scan")
		purego.RegisterLibFunc(&surrealVecUpsert, lib, "surreal_vec_upsert")
		purego.RegisterLibFunc(&surrealVecKnn, lib, "surreal_vec_knn")
		purego.RegisterLibFunc(&surrealVecSetMode, lib, "surreal_vec_set_mode")
		purego.RegisterLibFunc(&surrealGraphRelate, lib, "surreal_graph_relate")
		purego.RegisterLibFunc(&surrealGraphTraverse, lib, "surreal_graph_traverse")
		purego.RegisterLibFunc(&surrealFTSIndex, lib, "surreal_fts_index")
		purego.RegisterLibFunc(&surrealFTSSearch, lib, "surreal_fts_search")
		purego.RegisterLibFunc(&surrealFreeString, lib, "surreal_free_string")
		purego.RegisterLibFunc(&surrealFreeBuf, lib, "surreal_free_buf")
	})
	return surrealErr
}

// ─── FFI 辅助 ─────────────────────────────────────────────────────────────────

// bytePtrOrNil 返回 []byte 首字节指针；空 slice 返回 nil（Rust 侧 from_raw_parts(nil, 0) 安全）。
func bytePtrOrNil(b []byte) *byte {
	if len(b) == 0 {
		return nil
	}
	return &b[0]
}

// readCStringAndFree 拷贝 NUL-terminated C 字符串到 Go string，立即调用 surreal_free_string 归还。
func readCStringAndFree(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	s := goStringFromPtr(ptr)
	surrealFreeString(ptr)
	return s
}

func goStringFromPtr(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	var n uintptr
	for {
		if *(*byte)(unsafe.Pointer(ptr + n)) == 0 {
			break
		}
		n++
	}
	if n == 0 {
		return ""
	}
	out := make([]byte, n)
	src := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), n)
	copy(out, src)
	return string(out)
}

// readBytesAndFree 拷贝 Rust 分配的字节段到 Go []byte，立即调用 surreal_free_buf 归还。
// ADR-0011 风险段强调"立即拷贝 + 立即归还"模式以杜绝 use-after-free。
func readBytesAndFree(ptr uintptr, n uintptr) []byte {
	if ptr == 0 || n == 0 {
		return nil
	}
	out := make([]byte, n)
	src := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), n)
	copy(out, src)
	surrealFreeBuf(ptr, n)
	return out
}

// ─── SurrealDBCoreStore ───────────────────────────────────────────────────────

// SurrealDBCoreStore 实现 protocol.Store，通过 purego 调用 Rust SurrealCore FFI。
// 认知检索轴：KV + 向量近邻（Tier0=暴力扫描, Tier1+=HNSW）+ 图遍历 + 全文检索。
type SurrealDBCoreStore struct {
	useHNSW bool
}

var _ protocol.Store = (*SurrealDBCoreStore)(nil)

// OpenSurrealDBCore 初始化全局 SurrealCoreStore（幂等）。
// backend: "mem"（默认）或 "rocksdb"（要求 ≥16GB，dbPath 不可为空）。
// vecDim: HNSW 向量维度，需与嵌入模型一致（典型值 1536）。
// useHNSW 参数已废弃（SurrealDB MTREE 索引始终激活），保留供调用方兼容使用。
func OpenSurrealDBCore(backend, dbPath string, vecDim int, useHNSW bool) (*SurrealDBCoreStore, error) {
	if err := bindSurreal(); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "surreal load lib", err)
	}
	if rc := surrealOpen(backend, dbPath, int32(vecDim)); rc != 0 {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("surreal_open failed: code %d", rc))
	}
	return &SurrealDBCoreStore{useHNSW: useHNSW}, nil
}

// ─── protocol.Store 实现 ──────────────────────────────────────────────────────

// Get 读取 KV 值；键不存在返回 errors.ErrNotFound。
func (s *SurrealDBCoreStore) Get(_ context.Context, key []byte) ([]byte, error) {
	var outVal uintptr
	var outLen uintptr
	rc := surrealKvGet(bytePtrOrNil(key), uintptr(len(key)), &outVal, &outLen)
	runtime.KeepAlive(key)
	switch rc {
	case 0:
		return readBytesAndFree(outVal, outLen), nil
	case 1:
		return nil, perrors.ErrNotFound
	default:
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("surreal_kv_get: code %d", rc))
	}
}

// Put 写入键值对。
func (s *SurrealDBCoreStore) Put(_ context.Context, key, value []byte) error {
	rc := surrealKvPut(bytePtrOrNil(key), uintptr(len(key)), bytePtrOrNil(value), uintptr(len(value)))
	runtime.KeepAlive(key)
	runtime.KeepAlive(value)
	if rc != 0 {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("surreal_kv_put: code %d", rc))
	}
	return nil
}

// Delete 删除键。
func (s *SurrealDBCoreStore) Delete(_ context.Context, key []byte) error {
	rc := surrealKvDelete(bytePtrOrNil(key), uintptr(len(key)))
	runtime.KeepAlive(key)
	if rc != 0 {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("surreal_kv_delete: code %d", rc))
	}
	return nil
}

// Scan 返回前缀扫描迭代器。
func (s *SurrealDBCoreStore) Scan(_ context.Context, prefix []byte) (protocol.Iterator, error) {
	var outJSON uintptr
	rc := surrealKvScan(bytePtrOrNil(prefix), uintptr(len(prefix)), &outJSON)
	runtime.KeepAlive(prefix)
	if rc != 0 {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("surreal_kv_scan: code %d", rc))
	}
	jsonStr := readCStringAndFree(outJSON)
	pairs, err := parseKVPairsJSON(jsonStr)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "surreal scan parse", err)
	}
	return &surrealIterator{pairs: pairs, pos: -1}, nil
}

// BatchWrite 批量写入；MVP 无原子保证（内存 KV 无事务）。
func (s *SurrealDBCoreStore) BatchWrite(ctx context.Context, ops []protocol.Op) error {
	for _, op := range ops {
		switch op.Type {
		case protocol.OpPut:
			if err := s.Put(ctx, op.Key, op.Value); err != nil {
				return err
			}
		case protocol.OpDelete:
			if err := s.Delete(ctx, op.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

// Txn 伪事务：串行执行 fn，内存 KV 无回滚能力（MVP 限制）。
func (s *SurrealDBCoreStore) Txn(_ context.Context, fn func(tx protocol.Transaction) error) error {
	return fn(&surrealTx{store: s})
}

func (s *SurrealDBCoreStore) Capabilities() protocol.StoreCapabilities {
	engine := "surreal-core-ffi/brute-cosine"
	if s.useHNSW {
		engine = "surreal-core-ffi/hnsw"
	}
	return protocol.StoreCapabilities{
		SupportsSQL:      false,
		SupportsVector:   true,
		SupportsGraph:    true,
		SupportsFullText: true,
		Engine:           engine,
	}
}

func (s *SurrealDBCoreStore) UseHNSW() bool { return s.useHNSW }

func (s *SurrealDBCoreStore) Close() error { return nil }

// ─── 扩展接口（向量 / 图 / 全文）────────────────────────────────────────────────

// ScoredID 检索结果（带评分）。
type ScoredID struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// VecUpsert 写入或更新向量记录。
func (s *SurrealDBCoreStore) VecUpsert(id string, embedding []float32) error {
	if len(embedding) == 0 {
		return perrors.New(perrors.CodeInternal, "surreal_vec_upsert: empty embedding")
	}
	rc := surrealVecUpsert(id, &embedding[0], uintptr(len(embedding)))
	runtime.KeepAlive(embedding)
	if rc != 0 {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("surreal_vec_upsert: code %d", rc))
	}
	return nil
}

// VecKNN K 近邻向量检索（余弦相似度）。
func (s *SurrealDBCoreStore) VecKNN(query []float32, k int) ([]ScoredID, error) {
	if len(query) == 0 {
		return nil, perrors.New(perrors.CodeInternal, "surreal_vec_knn: empty query")
	}
	var outJSON uintptr
	rc := surrealVecKnn(&query[0], uintptr(len(query)), uintptr(k), &outJSON)
	runtime.KeepAlive(query)
	if rc != 0 {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("surreal_vec_knn: code %d", rc))
	}
	return parseScoredJSON(readCStringAndFree(outJSON))
}

// GraphRelate 写入有向图边 from -[edgeType]-> to。
func (s *SurrealDBCoreStore) GraphRelate(fromID, edgeType, toID string) error {
	rc := surrealGraphRelate(fromID, edgeType, toID)
	if rc != 0 {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("surreal_graph_relate: code %d", rc))
	}
	return nil
}

// GraphTraverse BFS 多跳图遍历；edgeType 为空串表示匹配所有边类型。
func (s *SurrealDBCoreStore) GraphTraverse(startID, edgeType string, maxDepth int) ([]string, error) {
	var outJSON uintptr
	rc := surrealGraphTraverse(startID, edgeType, uintptr(maxDepth), &outJSON)
	if rc != 0 {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("surreal_graph_traverse: code %d", rc))
	}
	return parseIDsJSON(readCStringAndFree(outJSON))
}

// FTSIndex 将文档写入全文检索倒排索引。
func (s *SurrealDBCoreStore) FTSIndex(docID, text string) error {
	rc := surrealFTSIndex(docID, text)
	if rc != 0 {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("surreal_fts_index: code %d", rc))
	}
	return nil
}

// FTSSearch TF-IDF 全文检索，返回 top-k 结果。
func (s *SurrealDBCoreStore) FTSSearch(query string, k int) ([]ScoredID, error) {
	var outJSON uintptr
	rc := surrealFTSSearch(query, uintptr(k), &outJSON)
	if rc != 0 {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("surreal_fts_search: code %d", rc))
	}
	return parseScoredJSON(readCStringAndFree(outJSON))
}

// ─── surrealTx — 伪事务（内存 KV 无真实回滚，MVP 限制）────────────────────────

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
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("parseKVPairs: %v", err), err)
	}
	pairs := make([]surrealKVPair, 0, len(raw))
	for _, p := range raw {
		k, err := hex.DecodeString(p.K)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("parseKVPairs key hex: %v", err), err)
		}
		v, err := hex.DecodeString(p.V)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("parseKVPairs val hex: %v", err), err)
		}
		pairs = append(pairs, surrealKVPair{Key: k, Value: v})
	}
	return pairs, nil
}

func parseScoredJSON(jsonStr string) ([]ScoredID, error) {
	var results []ScoredID
	if err := json.Unmarshal([]byte(jsonStr), &results); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("parseScoredJSON: %v", err), err)
	}
	return results, nil
}

func parseIDsJSON(jsonStr string) ([]string, error) {
	var ids []string
	if err := json.Unmarshal([]byte(jsonStr), &ids); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("parseIDsJSON: %v", err), err)
	}
	return ids, nil
}
