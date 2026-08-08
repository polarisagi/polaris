package guard

// 白盒测试专用访问器。
// 2026-08-08：三者原在 pii_desensitizer.go，唯一调用链是
// pii_desensitizer_test.go → partitionLen → lruMapping.len，生产侧零调用，
// 长期占据 deadcode 白名单。移入 _test.go 后既不进生产构建也不再需要豁免。

// partitionLen 仅供测试：返回指定分区当前映射条数（不存在则返回 0，不创建分区）。
func (d *PIIDesensitizer) partitionLen(partitionKey string) int {
	d.mu.Lock()
	mapping, ok := d.partitions[partitionKey]
	d.mu.Unlock()
	if !ok {
		return 0
	}
	return mapping.len()
}

// partitionCount 仅供测试：返回当前分区总数。
func (d *PIIDesensitizer) partitionCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.partitions)
}

func (m *lruMapping) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ll.Len()
}
