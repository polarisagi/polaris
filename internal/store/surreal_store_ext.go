package store

import (
	"encoding/json"
	"fmt"
	"math"
	"runtime"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── 扩展接口（向量 / 图 / 全文）（R7 拆分自 surreal_store.go）────────────────────
// FFI 绑定/protocol.Store 基础实现见 surreal_store.go；
// 伪事务/迭代器/JSON 解析辅助见 surreal_store_helpers.go。

// ScoredID 检索结果（带评分）。
type ScoredID struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// VecUpsert 写入或更新向量记录。
func (s *SurrealDBCoreStore) VecUpsert(id string, embedding []float32) error {
	if len(embedding) == 0 {
		return apperr.New(apperr.CodeInternal, "surreal_vec_upsert: empty embedding")
	}
	rc := surrealVecUpsert(id, &embedding[0], uintptr(len(embedding)))
	runtime.KeepAlive(embedding)
	if rc != 0 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("surreal_vec_upsert: code %d", rc))
	}
	return nil
}

// VecKNN K 近邻向量检索（余弦相似度）。
func (s *SurrealDBCoreStore) VecKNN(query []float32, k int) ([]ScoredID, error) {
	if len(query) == 0 {
		return nil, apperr.New(apperr.CodeInternal, "surreal_vec_knn: empty query")
	}
	var outJSON uintptr
	rc := surrealVecKnn(&query[0], uintptr(len(query)), uintptr(k), &outJSON)
	runtime.KeepAlive(query)
	if rc != 0 {
		if outJSON != 0 {
			surrealFreeString(outJSON)
		}
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("surreal_vec_knn: code %d", rc))
	}
	return parseScoredJSON(readCStringAndFree(outJSON))
}

// GraphRelate 写入有向图边 from -[edgeType]-> to。
func (s *SurrealDBCoreStore) GraphRelate(fromID, edgeType, toID string, weight float64) error {
	bFrom, pFrom, lFrom := strToBytes(fromID)
	bEt, pEt, lEt := strToBytes(edgeType)
	bTo, pTo, lTo := strToBytes(toID)

	rc := surrealGraphRelate(pFrom, lFrom, pEt, lEt, pTo, lTo, uintptr(math.Float64bits(weight)))

	runtime.KeepAlive(bFrom)
	runtime.KeepAlive(bEt)
	runtime.KeepAlive(bTo)

	if rc != 0 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("surreal_graph_relate: code %d", rc))
	}
	return nil
}

// GraphSpreadingActivation 蔓延激活图遍历
func (s *SurrealDBCoreStore) GraphSpreadingActivation(startIDs []string, maxDepth int, energyDecay float64, dormancyThreshold float64, fanOutLimit int) ([]ScoredID, error) {
	if len(startIDs) == 0 {
		return nil, apperr.New(apperr.CodeInternal, "surreal_graph_spreading_activation: empty startIDs")
	}
	startIDsJSON, err := json.Marshal(startIDs)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "surreal_graph_spreading_activation: marshal ids", err)
	}

	bIds, pIds, lIds := strToBytes(string(startIDsJSON))

	var outJSON uintptr
	rc := surrealGraphSpreadingActivation(
		pIds,
		lIds,
		uintptr(maxDepth),
		uintptr(math.Float64bits(energyDecay)),
		uintptr(math.Float64bits(dormancyThreshold)),
		uintptr(fanOutLimit),
		&outJSON,
	)
	runtime.KeepAlive(bIds)
	if rc != 0 {
		if outJSON != 0 {
			surrealFreeString(outJSON)
		}
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("surreal_graph_spreading_activation: code %d", rc))
	}
	return parseScoredJSON(readCStringAndFree(outJSON))
}

// SpreadingActivation 实现 protocol.GraphTraverser 接口：将底层 GraphSpreadingActivation
// 的 []ScoredID 结果转换为 []types.ScoredNode，屏蔽内部存储类型。
func (s *SurrealDBCoreStore) SpreadingActivation(startIDs []string, maxDepth int, energyDecay, dormancyThreshold float64, fanOutLimit int) ([]types.ScoredNode, error) {
	if len(startIDs) == 0 {
		return nil, nil
	}
	scored, err := s.GraphSpreadingActivation(startIDs, maxDepth, energyDecay, dormancyThreshold, fanOutLimit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to execute spreading activation", err)
	}
	out := make([]types.ScoredNode, len(scored))
	for i, r := range scored {
		out[i] = types.ScoredNode{ID: r.ID, Score: r.Score}
	}
	return out, nil
}

// GraphTraverse BFS 多跳图遍历；edgeType 为空串表示匹配所有边类型。
func (s *SurrealDBCoreStore) GraphTraverse(startID, edgeType string, maxDepth int) ([]string, error) {
	bStart, pStart, lStart := strToBytes(startID)
	bEt, pEt, lEt := strToBytes(edgeType)

	var outJSON uintptr
	rc := surrealGraphTraverse(pStart, lStart, pEt, lEt, uintptr(maxDepth), &outJSON)

	runtime.KeepAlive(bStart)
	runtime.KeepAlive(bEt)

	if rc != 0 {
		if outJSON != 0 {
			surrealFreeString(outJSON)
		}
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("surreal_graph_traverse: code %d", rc))
	}
	return parseIDsJSON(readCStringAndFree(outJSON))
}

// FTSIndex 将文档写入全文检索倒排索引。
func (s *SurrealDBCoreStore) FTSIndex(docID, text string) error {
	rc := surrealFTSIndex(docID, text)
	if rc != 0 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("surreal_fts_index: code %d", rc))
	}
	return nil
}

// FTSSearch BM25 全文检索，返回 top-k 结果。
func (s *SurrealDBCoreStore) FTSSearch(query string, k int) ([]ScoredID, error) {
	var outJSON uintptr
	rc := surrealFTSSearch(query, uintptr(k), &outJSON)
	if rc != 0 {
		if outJSON != 0 {
			surrealFreeString(outJSON)
		}
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("surreal_fts_search: code %d", rc))
	}
	return parseScoredJSON(readCStringAndFree(outJSON))
}

// VecDelete 从 HNSW 索引删除向量记录（供 Forget 路径调用）。
func (s *SurrealDBCoreStore) VecDelete(id string) error {
	rc := surrealVecDelete(id)
	if rc != 0 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("surreal_vec_delete: code %d", rc))
	}
	return nil
}

// FTSDelete 从 BM25 FTS 索引删除文档（供 Forget 路径调用）。
func (s *SurrealDBCoreStore) FTSDelete(docID string) error {
	rc := surrealFTSDelete(docID)
	if rc != 0 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("surreal_fts_delete: code %d", rc))
	}
	return nil
}

// GraphDeleteEdges 删除 fromID 的出边（供 Forget 路径清理图结构）。
// edgeType 为空串表示删除所有出边。
func (s *SurrealDBCoreStore) GraphDeleteEdges(fromID, edgeType string) error {
	bFrom, pFrom, lFrom := strToBytes(fromID)
	bEt, pEt, lEt := strToBytes(edgeType)

	rc := surrealGraphDeleteEdges(pFrom, lFrom, pEt, lEt)

	runtime.KeepAlive(bFrom)
	runtime.KeepAlive(bEt)

	if rc != 0 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("surreal_graph_delete_edges: code %d", rc))
	}
	return nil
}
