package tts

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

const (
	// edgeTTSWSURL Microsoft Edge TTS WebSocket 端点。
	// TrustedClientToken 为 Edge 浏览器内置的公开常量，已被众多开源项目使用。
	edgeTTSWSURL  = "wss://speech.platform.bing.com/consumer/speech/synthesize/readspeaker/edge/v1"
	edgeTTSToken  = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	edgeTTSOrigin = "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold"

	// edgeTTSOutputFmt 请求 24kHz 16-bit 单声道原始 PCM，避免引入 MP3 解码依赖。
	edgeTTSOutputFmt  = "raw-24khz-16bit-mono-pcm"
	edgeTTSSampleRate = 24000
)

// EdgeProvider 通过 Microsoft Edge TTS WebSocket API 合成语音。
// 特性：免费、无需 API 密钥、中国大陆可正常访问（speech.platform.bing.com 未被封锁）。
// 输出：标准 WAV（16-bit PCM 单声道 24kHz）。
type EdgeProvider struct {
	voice string // 声线，如 "zh-CN-XiaoxiaoNeural"
	rate  string // 语速，如 "+0%"
	pitch string // 音调，如 "+0Hz"
}

// NewEdgeProvider 返回 EdgeProvider。
// voice 为空时使用默认中文女声 zh-CN-XiaoxiaoNeural（晓晓，音质最佳）。
// 其他可选中文声线：zh-CN-YunxiNeural（云希，男）/ zh-CN-XiaoYiNeural（晓伊）。
func NewEdgeProvider(voice string) *EdgeProvider {
	if voice == "" {
		voice = "zh-CN-XiaoxiaoNeural"
	}
	return &EdgeProvider{voice: voice, rate: "+0%", pitch: "+0Hz"}
}

// Generate 调用 Edge TTS WebSocket 合成语音并返回 WAV 字节流。
func (p *EdgeProvider) Generate(ctx context.Context, text string) ([]byte, error) {
	connID := strings.ReplaceAll(uuid.New().String(), "-", "")
	wsURL := fmt.Sprintf("%s?TrustedClientToken=%s&ConnectionId=%s", edgeTTSWSURL, edgeTTSToken, connID)

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	hdr := http.Header{}
	hdr.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0")
	hdr.Set("Origin", edgeTTSOrigin)

	conn, _, err := dialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "edge-tts: dial failed", err)
	}
	defer conn.Close()

	// context 取消时通过设置读超时解除 ReadMessage 阻塞
	concurrent.SafeGo(ctx, "llm.tts.edge_cancel_watcher", func(ctx context.Context) {
		<-ctx.Done()
		_ = conn.SetReadDeadline(time.Now())
	})

	if err := edgeSendRequests(conn, p, text); err != nil {
		return nil, err
	}

	pcm, err := edgeReadAudio(ctx, conn)
	if err != nil {
		return nil, err
	}
	return encodeWAVFromPCM16(pcm, edgeTTSSampleRate)
}

// Close 实现 Provider 接口（EdgeProvider 无持久连接，空操作）。
func (p *EdgeProvider) Close() error { return nil }

// edgeSendRequests 向已建立的 WebSocket 连接发送 speech.config 和 SSML 两条消息。
func edgeSendRequests(conn *websocket.Conn, p *EdgeProvider, text string) error {
	ts := edgeTimestamp()
	reqID := strings.ReplaceAll(uuid.New().String(), "-", "")

	configMsg := fmt.Sprintf(
		"X-Timestamp:%s\r\nContent-Type: application/json; charset=utf-8\r\nPath: speech.config\r\n\r\n"+
			`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":%q}}}}`,
		ts, edgeTTSOutputFmt)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(configMsg)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "edge-tts: write config failed", err)
	}

	lang := edgeVoiceLang(p.voice)
	ssml := fmt.Sprintf(
		"<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='%s'>"+
			"<voice name='%s'><prosody pitch='%s' rate='%s' volume='+0%%'>%s</prosody></voice></speak>",
		lang, p.voice, p.pitch, p.rate, edgeEscapeXML(text))
	ssmlMsg := fmt.Sprintf(
		"X-RequestId:%s\r\nContent-Type: application/ssml+xml\r\nX-Timestamp:%s\r\nPath: ssml\r\n\r\n%s",
		reqID, ts, ssml)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(ssmlMsg)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "edge-tts: write ssml failed", err)
	}
	return nil
}

// edgeReadAudio 从 WebSocket 连接中读取所有音频帧，直到收到 "Path:turn.end"。
// 返回原始 PCM16 字节流（尚未包装 WAV 头）。
func edgeReadAudio(ctx context.Context, conn *websocket.Conn) ([]byte, error) {
	var audioBuf bytes.Buffer
loop:
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				break loop
			}
			return nil, apperr.Wrap(apperr.CodeInternal, "edge-tts: read failed", err)
		}
		switch msgType {
		case websocket.BinaryMessage:
			edgeAppendAudioFrame(&audioBuf, msg)
		case websocket.TextMessage:
			if strings.Contains(string(msg), "Path:turn.end") {
				break loop
			}
		}
	}
	if audioBuf.Len() == 0 {
		return nil, apperr.New(apperr.CodeInternal, "edge-tts: no audio received")
	}
	return audioBuf.Bytes(), nil
}

// edgeAppendAudioFrame 解析二进制帧并将 PCM 数据追加到 buf。
// 帧格式：2字节 header 长度（big-endian）+ header 文本 + 音频 PCM 字节。
func edgeAppendAudioFrame(buf *bytes.Buffer, msg []byte) {
	if len(msg) < 2 {
		return
	}
	headerLen := int(binary.BigEndian.Uint16(msg[:2]))
	if len(msg) < 2+headerLen {
		return
	}
	frameHeader := string(msg[2 : 2+headerLen])
	// 仅提取音频帧（Path:audio），忽略 metadata 等其他帧
	if strings.Contains(frameHeader, "Path:audio") || strings.Contains(frameHeader, "Path: audio") {
		buf.Write(msg[2+headerLen:])
	}
}

// edgeTimestamp 返回 Edge TTS 所需的 ISO 8601 毫秒格式时间戳。
func edgeTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// edgeVoiceLang 从声线名称提取语言标签，如 "zh-CN-XiaoxiaoNeural" → "zh-CN"。
func edgeVoiceLang(voice string) string {
	parts := strings.SplitN(voice, "-", 3)
	if len(parts) >= 2 {
		return parts[0] + "-" + parts[1]
	}
	return "zh-CN"
}

// edgeEscapeXML 对 SSML 文本内容进行 XML 转义，防注入。
func edgeEscapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}
