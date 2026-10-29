package surprise

import "container/list"

// DefaultTaskBucketCap 按 taskID 分桶的滑动均值容量上限。
// 单例计算器存活整个进程，taskID 只增不减，无界 map 会随会话数线性泄漏；
// 超限淘汰最久未更新的桶（LRU），被淘汰的任务再次读取时退回中性默认值。
const DefaultTaskBucketCap = 1024

// taskBucket 单个任务的 SurpriseIndex EWMA。
type taskBucket struct {
	taskID string
	avg    float64
	count  int64
}

// taskBuckets 有界 LRU 分桶。非并发安全，由 SurpriseCalculator.mu 保护。
type taskBuckets struct {
	limit int
	order *list.List // 队首 = 最近更新；元素值 *taskBucket
	idx   map[string]*list.Element
}

func newTaskBuckets(limit int) *taskBuckets {
	if limit <= 0 {
		limit = DefaultTaskBucketCap
	}
	return &taskBuckets{limit: limit, order: list.New(), idx: make(map[string]*list.Element, limit)}
}

// observe 以 EWMA(α=0.2) 并入一次结果；首个样本直接作为均值。
func (b *taskBuckets) observe(taskID string, result float64) {
	if el, ok := b.idx[taskID]; ok {
		tb := el.Value.(*taskBucket)
		tb.avg = 0.8*tb.avg + 0.2*result
		tb.count++
		b.order.MoveToFront(el)
		return
	}
	b.idx[taskID] = b.order.PushFront(&taskBucket{taskID: taskID, avg: result, count: 1})
	for b.order.Len() > b.limit {
		oldest := b.order.Back()
		b.order.Remove(oldest)
		delete(b.idx, oldest.Value.(*taskBucket).taskID)
	}
}

// get 读取任务的当前均值；不刷新 LRU 顺序（只读不应延长桶寿命）。
func (b *taskBuckets) get(taskID string) (float64, bool) {
	el, ok := b.idx[taskID]
	if !ok {
		return 0, false
	}
	return el.Value.(*taskBucket).avg, true
}

func (b *taskBuckets) size() int { return b.order.Len() }
