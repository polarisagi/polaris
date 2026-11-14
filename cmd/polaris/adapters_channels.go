package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/channelsadmin"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func matchPilotChannel(name string) (string, bool) {
	switch strings.ToLower(name) {
	case "telegram", "feishu", "wecom", "dingtalk", "qqbot", "discord", "slack", "whatsapp",
		"line", "teams", "email", "matrix", "mattermost", "signal", "sms", "homeassistant", "webhook":
		return strings.ToLower(name), true
	default:
		return "", false
	}
}

// wireChannelMutualExclusion 建立内置通道与插件通道之间的双向启用互斥。
func wireChannelMutualExclusion(ca *channelsadmin.ChannelsAdmin, pc *lifecycle.ChannelService, db *sql.DB) {
	if ca == nil || pc == nil || db == nil {
		return
	}

	// 启用插件通道时检查内置通道
	pc.SetConflictChecker(func(ctx context.Context, pluginID, pluginName, server string) error {
		builtInType, ok := matchPilotChannel(server)
		if !ok {
			return nil
		}
		var count int
		err := db.QueryRowContext(ctx, "SELECT COUNT(1) FROM channels WHERE enabled=1 AND LOWER(type)=LOWER(?)", builtInType).Scan(&count)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "check built-in channel conflict", err)
		}
		if count > 0 {
			return apperr.New(apperr.CodeConflict, fmt.Sprintf("内置 %s 通道已启用，请先停用内置通道再启用插件通道", server))
		}
		return nil
	})

	// 启用内置通道时检查插件通道
	ca.PluginChannelConflict = func(ctx context.Context, channelType string) error {
		pluginServer, ok := matchPilotChannel(channelType)
		if !ok {
			return nil
		}
		bindings, err := pc.ListBindings(ctx)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "list plugin channel bindings", err)
		}
		for _, b := range bindings {
			if strings.EqualFold(b.Server, pluginServer) && b.Enabled {
				return apperr.New(apperr.CodeConflict, fmt.Sprintf("插件通道 %s/%s 已启用，请先停用插件通道再启用内置通道", b.PluginName, b.Server))
			}
		}
		return nil
	}
}
