package network

import "time"

// safe_dialer_dnscache.go — SafeDialer 的 DNS 缓存容量治理（GR-2-002）。
//
// 从 safe_dialer.go 拆出的原因有两条，且第二条才是主因：
//  1. R7 单文件 400 行上限——淘汰逻辑内联后该文件到 416 行；
//  2. 淘汰策略是**独立可测**的关注点。它与拨号/CIDR 判定没有共享状态，
//     混在 resolveDNSBypass 里既看不清也没法单独构造边界用例。

// evictDNSCacheLocked 在写入新 key 前把缓存压回容量上限内。
//
// 调用方必须已持有 sd.dnsCacheMu 的写锁（函数名后缀 Locked 即此约定）。
//
// 策略：先淘汰任意一条已过 TTL 的条目；没有过期条目时淘汰时间戳最旧的一条。
// 每次插入只淘汰一条即可维持 len <= dnsCacheMax——因为容量只会被插入抬升 1。
//
// 为什么不引入 LRU 库：Tier-0（2GB VPS）约束下不为一个上千条目的小 map 增加
// 依赖；且 DNS 缓存的访问分布本就被 TTL 主导，严格 LRU 相对"最旧优先"没有
// 可测量的收益（A-06 只要求"有上限且会淘汰"，未要求特定淘汰算法）。
func (sd *SafeDialer) evictDNSCacheLocked(incomingHost string) {
	if sd.dnsCacheMax <= 0 || len(sd.dnsCache) < sd.dnsCacheMax {
		return
	}
	// 覆盖写不会增加条目数，无需淘汰。
	if _, exists := sd.dnsCache[incomingHost]; exists {
		return
	}

	now := time.Now()
	for k, ts := range sd.dnsCacheTs {
		if now.Sub(ts) >= sd.dnsCacheTTL {
			delete(sd.dnsCache, k)
			delete(sd.dnsCacheTs, k)
			return
		}
	}

	var oldestKey string
	var oldestTs time.Time
	for k, ts := range sd.dnsCacheTs {
		if oldestKey == "" || ts.Before(oldestTs) {
			oldestKey, oldestTs = k, ts
		}
	}
	if oldestKey != "" {
		delete(sd.dnsCache, oldestKey)
		delete(sd.dnsCacheTs, oldestKey)
	}
}
