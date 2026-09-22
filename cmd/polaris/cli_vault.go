package main

import (
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func runVaultCmd(args []string) error {
	if len(args) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "vault: missing subcommand (e.g. init, rotate-master-key)")
	}

	switch args[0] {
	case "init":
		return runVaultInit()
	case "rotate-master-key":
		return runVaultRotate()
	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("vault: unknown subcommand %q", args[0]))
	}
}

func runVaultInit() error {
	// resolveDataDirBase(nil)：vault 子命令不加载完整 config（与 benchmark.go 的
	// runBenchmarkRouting 同一模式），只识别 POLARIS_DATA_DIR env 覆盖，
	// 不识别 cfg.System.DataDir——避免 vault key 落在与 server 启动路径不同的
	// 硬编码 home 目录（Docker 部署下 $HOME 常非持久化卷）。
	dataDir, err := resolveDataDirBase(nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault init failed", err)
	}
	_, err = credential.NewVaultInDir(dataDir)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault init failed", err)
	}
	slog.Info("polaris: credential vault initialized successfully", "data_dir", dataDir)
	return nil
}

// runVaultRotate 通过 HTTP 触发内核执行主密钥轮换（ADR-0096 决策一）。
// 轮换本身（解密/重新加密/原子替换 vault.key）在内核侧
// internal/gateway/server/sysadmin/vault_admin.go 的 HandleVaultRotateMasterKey
// 完成——该操作必须与内核共享同一个 *sql.DB 写连接与同一份运行时 credential.Vault
// 实例，CLI 侧不再另开数据库连接。轮换完成后内核会自行重启以从新 vault.key 重新
// 加载，CLI 只需告知用户服务将短暂中断。
func runVaultRotate() error {
	if err := cliCheckServer(); err != nil {
		return err
	}
	var resp struct {
		Status           string `json:"status"`
		ProvidersRotated int    `json:"providers_rotated"`
		Restarting       bool   `json:"restarting"`
	}
	if err := cliPost("/v1/vault/rotate-master-key", nil, &resp); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "vault rotate-master-key failed", err)
	}
	slog.Info("polaris: credential master key rotated successfully", "providers_updated", resp.ProvidersRotated)
	fmt.Printf("%s  主密钥已轮换（%d 个 provider 已重新加密）\n", clr(ansiOk, "✓"), resp.ProvidersRotated)
	if resp.Restarting {
		fmt.Println(clr(ansiDim, "  守护进程正在重启以加载新密钥，片刻后恢复可用。"))
	}
	return nil
}
