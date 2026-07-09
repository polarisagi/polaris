package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/gorilla/websocket"
)

// Home Assistant 通过官方 WebSocket API 接入。
//
// 配置项：
//
//	url     string  HA 地址，如 "http://homeassistant.local:8123"
//	token   string  Long-Lived Access Token
//	watch_domains  string  逗号分隔的设备域名过滤，空=全部（如 "light,switch"）
//	watch_entities string  逗号分隔的实体 ID 过滤，空=全部
//	cooldown_seconds int   同一实体最短触发间隔秒数（默认 30）

func RunHomeAssistantPoller(ctx context.Context, host PollerHost, channelID, haURL, haToken string, cfg map[string]any) {
	slog.Info("homeassistant: poller started", "channel", channelID)
	defer slog.Info("homeassistant: poller stopped", "channel", channelID)

	backoff := 5 * time.Second
	for {
		if err := haConnect(ctx, host, channelID, haURL, haToken, cfg); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("homeassistant: connection error", "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

func haConnect(ctx context.Context, host PollerHost, channelID, haURL, haToken string, cfg map[string]any) error { //nolint:gocyclo
	// 将 http(s) 替换为 ws(s)
	wsURL := strings.Replace(haURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL = strings.TrimRight(wsURL, "/") + "/api/websocket"

	// 过滤配置
	watchDomains := make(map[string]bool)
	if wd, _ := cfg["watch_domains"].(string); wd != "" {
		for d := range strings.SplitSeq(wd, ",") {
			if d = strings.TrimSpace(d); d != "" {
				watchDomains[d] = true
			}
		}
	}
	watchEntities := make(map[string]bool)
	if we, _ := cfg["watch_entities"].(string); we != "" {
		for e := range strings.SplitSeq(we, ",") {
			if e = strings.TrimSpace(e); e != "" {
				watchEntities[e] = true
			}
		}
	}
	cooldown := 30 * time.Second
	if cs, _ := cfg["cooldown_seconds"].(float64); cs > 0 {
		cooldown = time.Duration(cs) * time.Second
	}
	lastEvent := make(map[string]time.Time)

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("ha: dial: %v", err), err)
	}
	defer conn.Close()

	var msgID atomic.Int64
	nextID := func() int64 { return msgID.Add(1) }

	// Auth 握手
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("ha: read auth_required: %v", err), err)
	}
	var authReq struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &authReq)
	if authReq.Type != "auth_required" {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("ha: expected auth_required, got %s", authReq.Type))
	}

	if err := conn.WriteJSON(map[string]string{"type": "auth", "access_token": haToken}); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("ha: auth write: %v", err), err)
	}
	_, raw, err = conn.ReadMessage()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("ha: read auth_ok: %v", err), err)
	}
	var authOK struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &authOK)
	if authOK.Type != "auth_ok" {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("ha: auth failed (type=%s)", authOK.Type))
	}

	// 订阅 state_changed 事件
	subID := nextID()
	if err := conn.WriteJSON(map[string]any{
		"id": subID, "type": "subscribe_events", "event_type": "state_changed",
	}); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("ha: subscribe write: %v", err), err)
	}

	slog.Info("homeassistant: connected and subscribed", "channel", channelID)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("ha: read: %v", err), err)
		}

		var frame struct {
			Type  string `json:"type"`
			Event *struct {
				EventType string `json:"event_type"`
				Data      struct {
					EntityID string `json:"entity_id"`
					NewState *struct {
						State      string         `json:"state"`
						Attributes map[string]any `json:"attributes"`
					} `json:"new_state"`
					OldState *struct {
						State string `json:"state"`
					} `json:"old_state"`
				} `json:"data"`
			} `json:"event"`
		}
		if json.Unmarshal(raw, &frame) != nil || frame.Type != "event" || frame.Event == nil {
			continue
		}
		if frame.Event.EventType != "state_changed" {
			continue
		}

		entityID := frame.Event.Data.EntityID
		newState := frame.Event.Data.NewState
		oldState := frame.Event.Data.OldState
		if newState == nil {
			continue
		}

		// 实体 / 域名过滤
		domain := ""
		if parts := strings.SplitN(entityID, ".", 2); len(parts) == 2 {
			domain = parts[0]
		}
		if len(watchEntities) > 0 && !watchEntities[entityID] {
			continue
		}
		if len(watchDomains) > 0 && !watchDomains[domain] {
			continue
		}

		// 忽略状态未变化的事件
		if oldState != nil && oldState.State == newState.State {
			continue
		}

		// 冷却时间限速
		if last, ok := lastEvent[entityID]; ok && time.Since(last) < cooldown {
			continue
		}
		lastEvent[entityID] = time.Now()

		// 构造可读消息
		friendlyName := entityID
		if fn, ok := newState.Attributes["friendly_name"].(string); ok && fn != "" {
			friendlyName = fn
		}
		var stateDesc string
		if oldState != nil {
			stateDesc = fmt.Sprintf("%s → %s", oldState.State, newState.State)
		} else {
			stateDesc = newState.State
		}
		text := fmt.Sprintf("[HA] %s (%s): %s", friendlyName, entityID, stateDesc)

		//custom-nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
		go host.OnMessage("homeassistant", channelID, cfg, Message{
			Text: text, ChatID: channelID, UserID: "homeassistant",

			TaintLevel: types.TaintHigh,
		})
	}
}

// HaSendPersistentNotification 通过 HA REST API 发送持久通知。
func HaSendPersistentNotification(ctx context.Context, client *http.Client, haURL, haToken, message string) error {
	url := strings.TrimRight(haURL, "/") + "/api/services/persistent_notification/create"
	body, _ := json.Marshal(map[string]string{
		"message": message,
		"title":   "Polaris Agent",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "HaSendPersistentNotification", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+haToken)
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "HaSendPersistentNotification", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("ha: notify status %d: %s", resp.StatusCode, b))
	}
	return nil
}
