package repo

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"

	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type SQLiteBudgetRepository struct {
	db *sql.DB
}

var _ repo.BudgetRepository = (*SQLiteBudgetRepository)(nil)

func NewSQLiteBudgetRepository(db *sql.DB) *SQLiteBudgetRepository {
	return &SQLiteBudgetRepository{db: db}
}

const budgetKey = "config:budget:monthly_usd"

func (r *SQLiteBudgetRepository) GetBudget(ctx context.Context) (float64, error) {
	var raw string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM kv_store WHERE key=?`, budgetKey).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("db error: %w", err)
	}
	val, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("parse error: %w", err)
	}
	return val, nil
}

func (r *SQLiteBudgetRepository) SetBudget(ctx context.Context, monthlyUSD float64) error {
	val := strconv.FormatFloat(monthlyUSD, 'f', 2, 64)
	_, err := r.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO kv_store(key, value, updated_at) VALUES(?,?,datetime('now'))`,
		budgetKey, val)
	if err != nil {
		return fmt.Errorf("db error: %w", err)
	}
	return nil
}
