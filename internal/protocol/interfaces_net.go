package protocol

import (
	"context"
	"net"

	"github.com/polarisagi/polaris/pkg/types"
)

type

// SafeDialer 是统一安全拨号器。
// 强制所有出站连接（HTTP/gRPC/WebSocket）使用，封装 SSRFGuard 五阶段校验:
//
//	Phase 0: Capability Token 出口强制
//	Phase 1: DNS 解析
//	Phase 2: blockedCIDRs 校验（内网地址段 + loopback 阻止）
//	Phase 3: 50ms TOCTOU 延迟后二次 DNS 解析 + 重新 CIDR 校验
//	Phase 3.5: 响应 IP 数 >20 → 拒绝
//	Phase 4: DNS TOCTOU 消除 —— 覆写 DialContext 锁定验证后的 IP
//
// M11 导出此接口，CI safe_dialer_lint 扫描裸 net.Dial/grpc.Dial/http.Get → ERROR。
SafeDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type

// Connector 是外部数据源的标准化接入接口。
Connector interface {
	ID() string
	Name() string
	List(ctx context.Context) ([]*types.DocumentRef, error)
	Fetch(ctx context.Context, ref *types.DocumentRef) (*types.SyncDocument, error)
	Watch(ctx context.Context) (<-chan types.ChangeEvent, error)
	SyncConfig() types.SyncConfig
}
