package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/gateway/mesh"
	"github.com/polarisagi/polaris/internal/store/repo"
)

// bootMesh 装配跨节点委派服务（ADR-0111）。a2a.enabled=false 时返回 nil：门面 404、
// 对端令牌一律拒绝、对端任务无人以 peer 角色执行（DefaultTaskWorker fail-closed）。
// 身份密钥读写失败只告警并关闭本功能，不阻断守护进程启动（Tier-0 可用性优先）。
func bootMesh(sb *SubstrateBundle, blackboard *orchestrator.SQLiteBlackboard) *mesh.Service {
	if !sb.Cfg.A2A.Enabled {
		slog.Info("polaris: cross-node delegation disabled (a2a.enabled=false)")
		return nil
	}
	self, err := mesh.GetOrCreateIdentity(sb.DataDir)
	if err != nil {
		slog.Error("polaris: mesh node identity unavailable, cross-node delegation disabled", "err", err)
		return nil
	}
	cfg := meshServiceConfig(sb.Cfg.Mesh)
	cfg.FileRoot = peerFileRoot(sb.DataDir, sb.Cfg.Mesh.PeerFileRoot)
	if err := os.MkdirAll(cfg.FileRoot, 0o700); err != nil {
		// 目录建不出来时工具对空目录 fail-closed（读不到任何文件），不放宽。
		slog.Warn("polaris: create mesh peer file root failed; peer tasks will not be able to read any file", "dir", cfg.FileRoot, "err", err)
	}
	svc := mesh.NewService(repo.NewSQLiteMeshRepository(sb.Store.DB()), blackboard, self, cfg)
	if sb.AuditTrail != nil {
		svc.WithAuditor(sb.AuditTrail)
	}
	if err := svc.Peers().Refresh(context.Background()); err != nil {
		slog.Warn("polaris: load mesh peers failed; outbound delegation to private addresses blocked until fixed", "err", err)
	}
	if sb.Dialer != nil {
		// ADR-0111 决策六第 2 条：只对 mesh_peers 登记的精确 host:port 放行，不按网段。
		sb.Dialer.SetPeerAllowlist(svc.Peers().Allowed)
	}
	if sb.EgressGW != nil {
		// 域名白名单层同样只放行登记过的对端（对端是 IP，不可能在域名白名单里）。
		sb.EgressGW.SetPeerAllowlist(svc.Peers().Allowed)
	}
	slog.Info("polaris: cross-node delegation enabled", "node_id", self.NodeID, "fingerprint", self.Fingerprint(),
		"extra_listen", sb.Cfg.Interface.ExtraListen, "peer_file_root", cfg.FileRoot)
	return svc
}

// peerFileRoot 对端任务的唯一文件根：配置值优先，空则 <data_dir>/workspace/peer-shared。
// 设置页热更新走 server.resolvePeerFileRoot，规则须与此一致。
func peerFileRoot(dataDir, configured string) string {
	if configured != "" {
		return filepath.Clean(configured)
	}
	return filepath.Join(dataDir, "workspace", "peer-shared")
}

func meshServiceConfig(c config.MeshConfig) mesh.Config {
	out := mesh.DefaultConfig()
	out.ReadOnly = c.PeerReadOnly
	out.AllowRelay = c.AllowRelay
	if c.ResultRetentionHours > 0 {
		out.Retention = time.Duration(c.ResultRetentionHours) * time.Hour
	}
	if c.MaxInputBytes > 0 {
		out.MaxInputBytes = c.MaxInputBytes
	}
	return out
}
