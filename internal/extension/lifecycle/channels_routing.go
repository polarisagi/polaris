package lifecycle

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/polarisagi/polaris/pkg/apperr"
)

const maxChannelRouteIDBytes = 256

type channelRoute struct {
	chatID   string
	userID   string
	threadID string
}

// validateChannelMessageID 校验 meta 中的 message_id（若存在）。
func validateChannelMessageID(meta map[string]string) (string, error) {
	if meta == nil {
		return "", nil
	}
	raw, ok := meta["message_id"]
	if !ok {
		return "", nil
	}
	id, err := normalizeChannelRouteID("message_id", raw, false)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInvalidInput, "channel route message_id", err)
	}
	return id, nil
}

// content-only Claude 事件沿用旧会话标识，避免不完整身份合并不同用户。
func channelRouteFromMeta(meta map[string]string) (*channelRoute, error) {
	if _, err := validateChannelMessageID(meta); err != nil {
		return nil, err
	}
	_, hasChat := meta["chat_id"]
	_, hasUser := meta["user_id"]
	_, hasThread := meta["thread_id"]
	if !hasChat && !hasUser && !hasThread {
		return nil, nil
	}
	chatID, err := normalizeChannelRouteID("chat_id", meta["chat_id"], false)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "channel route chat_id", err)
	}
	userID, err := normalizeChannelRouteID("user_id", meta["user_id"], false)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "channel route user_id", err)
	}
	threadID, err := normalizeChannelRouteID("thread_id", meta["thread_id"], true)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "channel route thread_id", err)
	}
	return &channelRoute{chatID: chatID, userID: userID, threadID: threadID}, nil
}

func normalizeChannelRouteID(field, value string, allowEmpty bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" && !allowEmpty {
		return "", apperr.New(apperr.CodeInvalidInput, field+" must be non-empty")
	}
	if len(value) > maxChannelRouteIDBytes || !utf8.ValidString(value) {
		return "", apperr.New(apperr.CodeInvalidInput, field+" has invalid length or encoding")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", apperr.New(apperr.CodeInvalidInput, field+" contains control characters")
		}
	}
	return value, nil
}

func routedChannelSessionID(pluginID, server string, route channelRoute) string {
	encoded := make([]byte, 0, 8*6+len(pluginID)+len(server)+len(route.chatID)+len(route.userID)+len(route.threadID)+len("polaris.channel.session.v1"))
	encoded = appendChannelRoutePart(encoded, "polaris.channel.session.v1")
	encoded = appendChannelRoutePart(encoded, pluginID)
	encoded = appendChannelRoutePart(encoded, server)
	encoded = appendChannelRoutePart(encoded, route.chatID)
	encoded = appendChannelRoutePart(encoded, route.userID)
	encoded = appendChannelRoutePart(encoded, route.threadID)
	digest := sha256.Sum256(encoded)
	return "ch_plugin_" + hex.EncodeToString(digest[:])
}

func appendChannelRoutePart(encoded []byte, value string) []byte {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	encoded = append(encoded, size[:]...)
	return append(encoded, value...)
}
