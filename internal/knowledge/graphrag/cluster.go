package graphrag

import (
	"context"
	"math"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ---------------------------------------------------------------------------
// CosineSimilarity 计算两个向量的余弦相似度。
func CosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0.0
	}
	var dotProduct, normA, normB float64
	for i := range a {
		valA, valB := float64(a[i]), float64(b[i])
		dotProduct += valA * valB
		normA += valA * valA
		normB += valB * valB
	}
	if normA == 0 || normB == 0 {
		return 0.0
	}
	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}

// ---------------------------------------------------------------------------
// Phase 4 — Clustering

// Clusterer 主题聚类器。
//
// Tier 0: Random Projection (4096→128d, JL 引理) →
//
//	Mini-Batch K-Means (k=√N) 或 BIRCH (CF-Tree 增量)。
//
// Tier 1+: DBSCAN (eps=0.3, minPts=5)。
//
//	实体 >5000 (Tier0) / >20000 (Tier1) → 降级 Mini-Batch K-Means。
type Clusterer struct {
	tier             int
	randomProjection *RandomProjection
	kmeans           *MiniBatchKMeans
	birch            *BIRCH
	dbscan           *DBSCAN
	leiden           *LeidenDetector
	summarizer       *CommunityGenerativeSummarizer // nil: Tier0 跳过摘要生成
}

// RandomProjection JL 引理降维 (4096→128d)。
type RandomProjection struct {
	projectionMatrix [][]float64
}

// Project 执行随机投影降维。
func (rp *RandomProjection) Project(vector []float32) []float32 {
	if len(rp.projectionMatrix) == 0 {
		return vector
	}
	res := make([]float32, len(rp.projectionMatrix))
	for i, row := range rp.projectionMatrix {
		var sum float64
		for j, val := range vector {
			if j < len(row) {
				sum += float64(val) * row[j]
			}
		}
		res[i] = float32(sum)
	}
	return res
}

// MiniBatchKMeans 小批量 K-Means (k=√N)。
type MiniBatchKMeans struct {
	k       int
	centers [][]float32
}

// GetK 返回聚类的类别数量 k。
func (mb *MiniBatchKMeans) GetK() int {
	return mb.k
}

// GetCenters 返回当前的聚类中心。
func (mb *MiniBatchKMeans) GetCenters() [][]float32 {
	return mb.centers
}

// BIRCH CF-Tree 增量聚类。
type BIRCH struct {
	cfTree *CFNode
}

// Insert 向 CF-Tree 插入一个点。
func (b *BIRCH) Insert(point []float64) {
	if b.cfTree == nil {
		b.cfTree = &CFNode{}
	}
	entry := &CFEntry{}
	entry.Update(point)
	b.cfTree.AddEntry(entry)
}

// CFNode CF-Tree 节点。
type CFNode struct {
	entries []*CFEntry
}

// AddEntry 添加一个子条目。
func (n *CFNode) AddEntry(entry *CFEntry) {
	n.entries = append(n.entries, entry)
}

// CFEntry CF 条目。
type CFEntry struct {
	n  int
	ls []float64 // linear sum
	ss float64   // square sum
}

// Update 更新 CF 特征。
func (e *CFEntry) Update(point []float64) {
	e.n++
	if len(e.ls) == 0 {
		e.ls = make([]float64, len(point))
	}
	for i, val := range point {
		e.ls[i] += val
		e.ss += val * val
	}
}

// DBSCAN 密度聚类。
type DBSCAN struct {
	eps    float64 // 0.3
	minPts int     // 5
}

// LeidenDetector Leiden 图社区检测。
type LeidenDetector struct {
	adjacencyMatrix [][]float64
}

// SetAdjacency 设置图的邻接矩阵。
func (ld *LeidenDetector) SetAdjacency(adj [][]float64) {
	ld.adjacencyMatrix = adj
}

// NewClusterer 按 tier 初始化聚类器。
func NewClusterer(tier int) *Clusterer {
	c := &Clusterer{tier: tier}
	if tier >= 1 {
		c.dbscan = &DBSCAN{eps: 0.3, minPts: 5}
		c.leiden = &LeidenDetector{}
	} else {
		c.randomProjection = &RandomProjection{}
		c.kmeans = &MiniBatchKMeans{}
		c.birch = &BIRCH{}
	}
	return c
}

// WithSummarizer 注入社区摘要生成器（可选；nil 或 Tier0 时 Cluster() 跳过摘要生成，
// 见 Clusterer.summarizer 字段注释）。
//
// 2026-07-08 恢复接线（复核 code-quality-remediation-verification-20260707.md
// Phase 1.3 遗留项时发现）：原 WithSummarizer 曾被判定为死代码删除，但
// CommunityGenerativeSummarizer 本身是完整实现且有专属测试覆盖
// （community_summarizer_test.go）的功能，Cluster() 内的调用点
// （c.summarizer != nil 分支）也从未被移除——真正缺失的只是这一个外部注入口，
// 导致该功能在生产环境永久不可达。详见 local_playground/reports/
// phase4-hard-dep-and-deadcode-followup-20260708.md。生产装配见
// cmd/polaris/boot_knowledge.go。
func (c *Clusterer) WithSummarizer(s *CommunityGenerativeSummarizer) {
	c.summarizer = s
}

// Cluster 执行完整聚类流程（包含 Leiden 检测与摘要生成）。
func (c *Clusterer) Cluster(ctx context.Context, gw *GraphWriter, entities []*Entity, adjacency [][]float64) ([]int, error) {
	if c.leiden == nil {
		return c.ClusterEntities(collectEmbeddings(entities)), nil
	}

	c.leiden.SetAdjacency(adjacency)
	labels := c.leiden.DetectCommunities(adjacency)

	if c.summarizer != nil && gw != nil {
		communities := make(map[int][]string)
		// 同步计算每个社区的最高污点级别，确保摘要节点不低于其成员的最高污点
		communityMaxTaint := make(map[int]types.TaintLevel)
		for i, label := range labels {
			communities[label] = append(communities[label], entities[i].Name)
			if entities[i].TaintLevel > communityMaxTaint[label] {
				communityMaxTaint[label] = entities[i].TaintLevel
			}
		}

		summaries, err := c.summarizer.Summarize(ctx, communities)
		if err != nil {
			return labels, apperr.Wrap(apperr.CodeInternal, "Clusterer.Cluster", err)
		}

		for _, s := range summaries {
			props := map[string]any{
				"summary":  s.Summary,
				"keywords": s.Keywords,
				"node_ids": s.NodeIDs,
			}
			entity := &Entity{
				ID:         "community:leiden:" + string(rune(s.CommunityID)),
				Name:       "Community Summary",
				Type:       "Community",
				Properties: props,
				TaintLevel: communityMaxTaint[s.CommunityID], // 继承成员最高污点，防止外部数据洗白
			}
			if err := gw.UpsertEntity(ctx, entity); err != nil {
				return labels, apperr.Wrap(apperr.CodeInternal, "Clusterer.Cluster", err)
			}
		}
	}
	return labels, nil
}

// ClusterEntities 按 tier 选择聚类算法，返回每个实体所属的聚类 ID（-1=噪声/未分类）。
// Tier0：Mini-Batch K-Means；Tier1+：DBSCAN（余弦距离）。
func (c *Clusterer) ClusterEntities(embeddings [][]float32) []int {
	if len(embeddings) == 0 {
		return nil
	}
	if c.tier >= 1 && len(embeddings) <= 20000 {
		return c.dbscan.Cluster(embeddings)
	}
	return c.kmeansCluster(embeddings)
}

// kmeansCluster Mini-Batch K-Means（k=√N，最多迭代 100 轮）。
func (c *Clusterer) kmeansCluster(embeddings [][]float32) []int { //nolint:gocyclo
	n := len(embeddings)
	k := int(math.Sqrt(float64(n)))
	if k < 1 {
		k = 1
	}
	if k > n {
		k = n
	}
	dim := len(embeddings[0])
	if dim == 0 {
		return make([]int, n)
	}

	centers := make([][]float32, k)
	for i := 0; i < k; i++ {
		centers[i] = make([]float32, dim)
		copy(centers[i], embeddings[i*(n/k)])
	}

	labels := make([]int, n)
	for iter := 0; iter < 100; iter++ {
		changed := false
		for i, e := range embeddings {
			best, bestSim := 0, -2.0
			for ci, center := range centers {
				sim := CosineSimilarity(e, center)
				if sim > bestSim {
					bestSim = sim
					best = ci
				}
			}
			if labels[i] != best {
				labels[i] = best
				changed = true
			}
		}
		if !changed {
			break
		}
		newCenters := make([][]float32, k)
		counts := make([]int, k)
		for i := range newCenters {
			newCenters[i] = make([]float32, dim)
		}
		for i, e := range embeddings {
			cl := labels[i]
			counts[cl]++
			for d := range e {
				newCenters[cl][d] += e[d]
			}
		}
		for ci := range newCenters {
			if counts[ci] > 0 {
				for d := range newCenters[ci] {
					newCenters[ci][d] /= float32(counts[ci])
				}
				centers[ci] = newCenters[ci]
			}
		}
	}
	return labels
}

// DBSCAN.Cluster/regionQuery、LeidenDetector.DetectCommunities 见
// cluster_algorithms.go（R7 拆分）。
