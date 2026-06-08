package swarm

import "math"

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

// ---------------------------------------------------------------------------
// DBSCAN 实现（余弦距离，eps=0.3, minPts=5）

// Cluster 执行 DBSCAN 聚类。返回标签数组（-1=噪声，>=0=聚类 ID）。
func (d *DBSCAN) Cluster(points [][]float32) []int {
	n := len(points)
	labels := make([]int, n)
	for i := range labels {
		labels[i] = -2 // -2=未访问
	}

	clusterID := 0
	for i := 0; i < n; i++ {
		if labels[i] != -2 {
			continue
		}
		neighbors := d.regionQuery(points, i)
		if len(neighbors) < d.minPts {
			labels[i] = -1
			continue
		}
		labels[i] = clusterID
		seed := make([]int, len(neighbors))
		copy(seed, neighbors)
		for j := 0; j < len(seed); j++ {
			nb := seed[j]
			if labels[nb] == -1 {
				labels[nb] = clusterID
			}
			if labels[nb] != -2 {
				continue
			}
			labels[nb] = clusterID
			nbNeighbors := d.regionQuery(points, nb)
			if len(nbNeighbors) >= d.minPts {
				seed = append(seed, nbNeighbors...)
			}
		}
		clusterID++
	}
	for i := range labels {
		if labels[i] == -2 {
			labels[i] = -1
		}
	}
	return labels
}

// regionQuery 返回与 points[idx] 余弦距离 ≤ eps 的所有点的 index。
func (d *DBSCAN) regionQuery(points [][]float32, idx int) []int {
	var result []int
	for i, p := range points {
		if i == idx {
			continue
		}
		if 1.0-CosineSimilarity(points[idx], p) <= d.eps {
			result = append(result, i)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Leiden 图社区检测（贪婪模块度优化，近似 Louvain）

// DetectCommunities 基于邻接矩阵做贪婪社区检测。返回节点 → 社区 ID 的映射。
// 算法：Louvain 第一阶段（局部模块度最大化），最多迭代 20 轮。
func (ld *LeidenDetector) DetectCommunities(adjacency [][]float64) []int { //nolint:gocyclo
	n := len(adjacency)
	if n == 0 {
		return nil
	}
	community := make([]int, n)
	for i := range community {
		community[i] = i
	}
	var m float64
	degree := make([]float64, n)
	for i := range adjacency {
		for j, w := range adjacency[i] {
			degree[i] += w
			if j > i {
				m += w
			}
		}
	}
	if m == 0 {
		return community
	}

	for iter := 0; iter < 20; iter++ {
		improved := false
		for i := 0; i < n; i++ {
			bestC := community[i]
			bestGain := 0.0

			neighborComs := map[int]float64{}
			for j, w := range adjacency[i] {
				if w > 0 && j != i {
					neighborComs[community[j]] += w
				}
			}
			curCom := community[i]
			for c, sumIn := range neighborComs {
				if c == curCom {
					continue
				}
				gain := 2*sumIn/m - degree[i]*degree[i]/(2*m*m)
				if gain > bestGain {
					bestGain = gain
					bestC = c
				}
			}
			if bestC != community[i] {
				community[i] = bestC
				improved = true
			}
		}
		if !improved {
			break
		}
	}

	idMap := map[int]int{}
	nextID := 0
	result := make([]int, n)
	for i, c := range community {
		if _, ok := idMap[c]; !ok {
			idMap[c] = nextID
			nextID++
		}
		result[i] = idMap[c]
	}
	return result
}
