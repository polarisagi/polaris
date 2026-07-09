package channelsadmin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/protocol/repo"
)

// ChannelConfig 聊天平台集成配置。config_json 存储厂商特有字段。
type ChannelConfig struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	Enabled       bool           `json:"enabled"`
	Config        map[string]any `json:"config"`
	WebhookSecret string         `json:"webhook_secret"`
	WebhookURL    string         `json:"webhook_url"` // 只读，由服务器生成
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`
}

func (h *ChannelsAdmin) HandleListChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.QueryContext(r.Context(),
		`SELECT id,name,type,enabled,config_json,webhook_secret,created_at,updated_at FROM channels ORDER BY created_at`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	list := []*ChannelConfig{}
	for rows.Next() {
		c := &ChannelConfig{}
		var enabled int
		var cfgJSON string
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &enabled, &cfgJSON, &c.WebhookSecret, &c.CreatedAt, &c.UpdatedAt); err != nil {
			continue
		}
		c.Enabled = enabled == 1
		json.Unmarshal([]byte(cfgJSON), &c.Config) //nolint:errcheck
		if c.Config == nil {
			c.Config = map[string]any{}
		}
		c.WebhookURL = webhookURL(c.Type, c.ID)
		c.WebhookSecret = "" // 不下发给前端
		list = append(list, c)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"channels": list}) //nolint:errcheck
}

func (h *ChannelsAdmin) HandleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var c ChannelConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if c.ID == "" {
		b := make([]byte, 8)
		rand.Read(b) //nolint:errcheck
		c.ID = "ch_" + hex.EncodeToString(b)
	}
	if c.WebhookSecret == "" {
		b := make([]byte, 16)
		rand.Read(b) //nolint:errcheck
		c.WebhookSecret = hex.EncodeToString(b)
	}
	cfgBytes, _ := json.Marshal(c.Config)
	now := time.Now().UTC().Format(time.RFC3339)

	err := h.ChannelRepo.CreateChannel(r.Context(), repo.ChannelRow{
		ID:            c.ID,
		Name:          c.Name,
		Type:          c.Type,
		Enabled:       c.Enabled,
		ConfigJSON:    string(cfgBytes),
		WebhookSecret: c.WebhookSecret,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if c.Enabled {
		h.ChannelMgr.Start(c.ID, c.Type, c.Config)
	}

	c.CreatedAt, c.UpdatedAt = now, now
	c.WebhookURL = webhookURL(c.Type, c.ID)
	c.WebhookSecret = ""
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func (h *ChannelsAdmin) HandleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channelID")
	var c ChannelConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfgBytes, _ := json.Marshal(c.Config)
	now := time.Now().UTC().Format(time.RFC3339)

	updated, err := h.ChannelRepo.UpdateChannel(r.Context(), repo.ChannelRow{
		ID:         id,
		Name:       c.Name,
		Type:       c.Type,
		Enabled:    c.Enabled,
		ConfigJSON: string(cfgBytes),
		UpdatedAt:  now,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !updated {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	h.ChannelMgr.Stop(id)
	if c.Enabled {
		h.ChannelMgr.Start(id, c.Type, c.Config)
	}

	c.ID = id
	c.UpdatedAt = now
	c.WebhookURL = webhookURL(c.Type, c.ID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func (h *ChannelsAdmin) HandleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channelID")
	h.ChannelMgr.Stop(id)
	err := h.ChannelRepo.DeleteChannel(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}) //nolint:errcheck
}

// webhookURL 生成平台 webhook 接收地址（纯函数，无需接收者）。
func webhookURL(channelType, channelID string) string {
	return "/v1/webhooks/" + channelType + "/" + channelID
}
