package repo

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// VaultRekeyer 旧密文 → 新密文（旧 vault 解密、新 vault 加密），由调用方持有两把密钥。
type VaultRekeyer func(ciphertext string) (string, error)

// VaultRekeyCounts 轮换改写的密文条数；providers 单列，保持管理接口 providers_rotated 语义。
type VaultRekeyCounts struct {
	Providers int
	Others    int
}

// vaultCipherColumn 一个整列存 Vault 密文的位置；行统一按 rowid 定位（各表主键形态不一，含复合键）。
type vaultCipherColumn struct {
	table, column, where string
}

// vaultCipherColumns 全部整列密文。新增 Vault 密文列必须登记在此，否则主密钥轮换后该列永久失效。
// mcp_servers.oauth 是 JSON 内嵌字段，由 rekeyMCPServerOAuth 单独处理。
func vaultCipherColumns() []vaultCipherColumn {
	return []vaultCipherColumn{
		{table: "providers", column: "api_key", where: "api_key != ''"},
		{table: "plugin_user_config", column: "value", where: "sensitive = 1 AND value != ''"},
		{table: "mcp_oauth_clients", column: "client_secret_enc", where: "client_secret_enc != ''"},
		{table: "mcp_oauth_tokens", column: "access_token_enc", where: "access_token_enc != ''"},
		{table: "mcp_oauth_tokens", column: "refresh_token_enc", where: "refresh_token_enc != ''"},
	}
}

// RekeyVaultCiphertexts 单事务改写库内全部 Vault 密文。必须单事务：逐条提交时中途失败会让已改写
// 的行使用随后被丢弃的新 key，永久无法解密。
func RekeyVaultCiphertexts(ctx context.Context, db *sql.DB, rekey VaultRekeyer) (VaultRekeyCounts, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return VaultRekeyCounts{}, apperr.Wrap(apperr.CodeInternal, "vault rekey: begin", err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交后 Rollback 为空操作；失败路径的回滚错误无可补救
	var counts VaultRekeyCounts
	for _, col := range vaultCipherColumns() {
		n, err := rekeyColumn(ctx, tx, col, rekey)
		if err != nil {
			return VaultRekeyCounts{}, err
		}
		if col.table == "providers" {
			counts.Providers += n
		} else {
			counts.Others += n
		}
	}
	n, err := rekeyMCPServerOAuth(ctx, tx, rekey)
	if err != nil {
		return VaultRekeyCounts{}, err
	}
	counts.Others += n
	if err := tx.Commit(); err != nil {
		return VaultRekeyCounts{}, apperr.Wrap(apperr.CodeInternal, "vault rekey: commit", err)
	}
	return counts, nil
}

type rekeyItem struct {
	key string
	ct  string
}

// collectRekeyItems 先读完再改写：SQLite 同一事务内边遍历游标边 UPDATE 同表会互相干扰。
func collectRekeyItems(ctx context.Context, tx *sql.Tx, query, what string) ([]rekeyItem, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "vault rekey: select "+what, err)
	}
	defer rows.Close()
	var items []rekeyItem
	for rows.Next() {
		var it rekeyItem
		if err := rows.Scan(&it.key, &it.ct); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "vault rekey: scan "+what, err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "vault rekey: iterate "+what, err)
	}
	return items, nil
}

func rekeyColumn(ctx context.Context, tx *sql.Tx, col vaultCipherColumn, rekey VaultRekeyer) (int, error) {
	what := col.table + "." + col.column
	// 表名/列名来自 vaultCipherColumns 静态登记表，非外部输入。
	items, err := collectRekeyItems(ctx, tx, "SELECT CAST(rowid AS TEXT), "+col.column+" FROM "+col.table+" WHERE "+col.where, what) //nolint:gosec // 静态登记表
	if err != nil {
		return 0, err
	}
	for _, it := range items {
		out, err := rekey(it.ct)
		if err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "vault rekey: "+what, err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE "+col.table+" SET "+col.column+"=? WHERE rowid=?", out, it.key); err != nil { //nolint:gosec // 静态登记表
			return 0, apperr.Wrap(apperr.CodeInternal, "vault rekey: update "+what, err)
		}
	}
	return len(items), nil
}

// rekeyMCPServerOAuth mcp_servers.oauth JSON 内的 client_secret_enc（预注册客户端密钥）。按通用 JSON
// 对象改写单个字段，其余字段原样保留——store 层不依赖扩展层的配置类型。
func rekeyMCPServerOAuth(ctx context.Context, tx *sql.Tx, rekey VaultRekeyer) (int, error) {
	items, err := collectRekeyItems(ctx, tx, `SELECT id, oauth FROM mcp_servers WHERE oauth LIKE '%client_secret_enc%'`, "mcp_servers.oauth")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(it.ct), &obj); err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "vault rekey: parse mcp_servers.oauth "+it.key, err)
		}
		var secret string
		if raw, ok := obj["client_secret_enc"]; !ok || json.Unmarshal(raw, &secret) != nil || secret == "" {
			continue
		}
		out, err := rekey(secret)
		if err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "vault rekey: mcp_servers.oauth "+it.key, err)
		}
		if obj["client_secret_enc"], err = json.Marshal(out); err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "vault rekey: encode mcp_servers.oauth "+it.key, err)
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "vault rekey: encode mcp_servers.oauth "+it.key, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mcp_servers SET oauth=? WHERE id=?`, string(b), it.key); err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "vault rekey: update mcp_servers.oauth "+it.key, err)
		}
		n++
	}
	return n, nil
}
