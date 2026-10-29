package mesh

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol/repo"
)

// Repository 委派服务所需的持久化能力（接口在消费方定义；SQL 实现为
// internal/store/repo.SQLiteMeshRepository，gateway 层不直接持有 *sql.DB）。
type Repository interface {
	InsertMeshToken(ctx context.Context, row repo.MeshTokenRow, tokenHash string) error
	FindMeshTokenByHash(ctx context.Context, tokenHash string) (*repo.MeshTokenRow, error)
	TouchMeshToken(ctx context.Context, tokenID string, usedAtMs int64) error
	RevokeMeshToken(ctx context.Context, tokenID string, revokedAtMs int64) error
	ListMeshTokens(ctx context.Context) ([]repo.MeshTokenRow, error)
	RebindMeshToken(ctx context.Context, peerName, fromNodeID, toNodeID string) (string, error)

	InsertMeshTask(ctx context.Context, row repo.MeshTaskRow) error
	UpdateMeshTaskState(ctx context.Context, taskID, state, statusMessage, result string, updatedAtMs int64) error
	MarkMeshTaskCanceled(ctx context.Context, taskID string) error
	DeleteExpiredMeshTasks(ctx context.Context, nowMs int64) error
	CountMeshTasks(ctx context.Context, f repo.MeshTaskFilter) (int, error)
	ListMeshTasks(ctx context.Context, f repo.MeshTaskFilter) ([]repo.MeshTaskRow, error)

	UpsertMeshPeer(ctx context.Context, p repo.MeshPeerRow) error
	DeleteMeshPeer(ctx context.Context, name string) error
	UpdateMeshPeerSeen(ctx context.Context, name string, seenAtMs int64, errMsg string) error
	ListMeshPeers(ctx context.Context, name string) ([]repo.MeshPeerRow, error)
}
