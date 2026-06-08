package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/cognition/memory"
)

type SQLPreferencesRepo struct {
	db *sql.DB
}

func NewSQLPreferencesRepo(db *sql.DB) *SQLPreferencesRepo {
	return &SQLPreferencesRepo{db: db}
}

func (r *SQLPreferencesRepo) GetPermissionMode(ctx context.Context) (protocol.PermissionMode, error) {
	var val string
	err := r.db.QueryRowContext(ctx, "SELECT value FROM preferences WHERE key = 'permission_mode'").Scan(&val)
	if err != nil {
		return protocol.ModeAutoReview, err // default
	}
	return protocol.PermissionMode(val), nil
}

func (r *SQLPreferencesRepo) SetPermissionMode(ctx context.Context, mode protocol.PermissionMode) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO preferences(key, value) VALUES('permission_mode', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		string(mode))
	return err
}

func LoadAllPreferences(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM preferences`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	prefs := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		prefs[k] = v
	}
	return prefs, rows.Err()
}

func (s *Server) handleGetPreferences(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT key, value FROM preferences`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	prefs := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		prefs[k] = v
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(prefs)
}

func (s *Server) handleSetPreference(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO preferences(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, req.Value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Hot reload preference in Agent
	s.agent.SetPreferences(map[string]string{key: req.Value})
	if s.agent.Memory() != nil {
		if ic, ok := s.agent.Memory().Working().Immutable().(*memory.ImmutableCore); ok {
			switch key {
			case "system_prompt", "global_goal":
				ic.GlobalGoal = req.Value
			case "system_prompt_template":
				ic.SystemPromptTemplate = req.Value
			case "custom_instructions":
				ic.CustomInstructions = req.Value
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "key": key, "value": req.Value})
}
