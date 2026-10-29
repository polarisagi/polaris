package httputil

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// WriteJSON 把 v 序列化为 JSON 写入响应体，编码/写入失败只记日志。
//
// 为什么需要这个 helper（2026-08-06，errcheck 存量治理）：
// 全仓曾有 81 处 `_ = json.NewEncoder(w).Encode(v)`，占 HE-1 静默吞错
// baseline 的 37%。这些点**不适合逐个展开错误处理**——响应写入失败几乎只有
// 一种成因（客户端已断开），handler 此时既不能改状态码（header 已发出）
// 也不能重试，能做的只有记一条日志。把这段三行模板复制 81 遍，是往代码里
// 灌噪声，且真出问题时 81 处各写各的日志格式，反而不好排查。
//
// 收敛成单点后：错误处理只有一份；将来若要给"响应写入失败"加指标或采样，
// 也只需改这里一处。
//
// 调用约定：调用方负责在此之前写好 status code 与除 Content-Type 外的
// 响应头（本函数会设置 Content-Type，故必须在 WriteHeader 之前调用，
// 或由调用方自行确保顺序正确）。
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Warn 而非 Error：绝大多数成因是客户端提前断开，属正常网络事件，
		// 按 Error 上报会淹没真实故障。
		slog.Warn("http: failed to write JSON response body", "err", err)
	}
}

// WriteJSONStatus 先写状态码再写 JSON 响应体。
//
// 单独提供而不让调用方自己 WriteHeader + WriteJSON：Header().Set 必须发生在
// WriteHeader 之前，否则 Content-Type 不会生效（net/http 在 WriteHeader 时
// 快照 header）。这个顺序约束容易写错且错了不报错——只是响应少了
// Content-Type，客户端按 text/plain 解析，表现为"接口偶尔返回乱码"。
func WriteJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("http: failed to write JSON response body", "status", status, "err", err)
	}
}
