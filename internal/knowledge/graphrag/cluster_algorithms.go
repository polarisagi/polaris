package graphrag

// ---------------------------------------------------------------------------
// DBSCAN 实现（余弦距离，eps=0.3, minPts=5）+ Leiden 图社区检测
// （R7 拆分自 cluster.go；DBSCAN/LeidenDetector 结构体定义与 Clusterer/
// RandomProjection/MiniBatchKMeans/BIRCH 见 cluster.go）

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
