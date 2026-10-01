package tts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/security/network"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

const (
	// edgeTTSWSURL Microsoft Edge TTS WebSocket 端点。
	// 路径必须是 readaloud（旧代码写成 readspeaker 会直接 HTTP 400，T1）。
	// TrustedClientToken 为 Edge 浏览器内置的公开常量，已被众多开源项目使用。
	edgeTTSWSURL  = "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1"
	edgeTTSToken  = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	edgeTTSOrigin = "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold"

	// edgeChromiumFullVersion 是伪装的 Edge/Chromium 完整版本号，同时进入 UA 与
	// Sec-MS-GEC-Version。微软升版本时可经 inference.tts.edge_client_version 覆盖，免发版。
	edgeChromiumFullVersion = "143.0.3650.75"

	// edgeTTSOutputFmt 该免费端点唯一实测可用的格式（T3）：raw-*-pcm 会被 1007 拒绝
	// （Unsupported Edge output format），因此输出直接是 MP3，不再做 PCM→WAV 封装。
	edgeTTSOutputFmt = "audio-24khz-48kbitrate-mono-mp3"

	// winEpochSeconds 是 1601-01-01 到 1970-01-01 的秒数（Windows FILETIME 纪元）。
	winEpochSeconds = 11644473600
)

// EdgeProvider 通过 Microsoft Edge TTS WebSocket API 合成语音。
// 特性：免费、无需 API 密钥、中国大陆可正常访问（speech.platform.bing.com 未被封锁）。
// 输出：MP3（audio-24khz-48kbitrate-mono-mp3）。
type EdgeProvider struct {
	safeDialer    *network.SafeDialer
	voice         string // 声线，如 "zh-CN-XiaoxiaoNeural"
	rate          string // 语速，如 "+0%"
	pitch         string // 音调，如 "+0Hz"
	clientVersion string // 伪装的 Chromium 完整版本号

	// clockSkew 是本机时钟相对微软服务器的偏差（秒）。Sec-MS-GEC 按 5 分钟取整的服务器
	// 时间计算，本机时钟偏差超过窗口即 403；收到 403 后按响应 Date 头校正并重试一次。
	// 放在实例上而非包级变量（internal/ 禁全局可变变量）：进程内只有一个 EdgeProvider，
	// 校准状态天然进程唯一；atomic 保证 Generate 并发安全。
	clockSkew atomic.Int64
}

// NewEdgeProvider 返回 EdgeProvider。
// voice 为空时使用默认中文女声 zh-CN-XiaoxiaoNeural（晓晓，音质最佳）。
// clientVersion 为空时使用 edgeChromiumFullVersion。
// 不再接收 style：免费端点不支持 mstts:express-as（服务端返回 "SSML is invalid"，T4）。
func NewEdgeProvider(voice, clientVersion string, safeDialer *network.SafeDialer) *EdgeProvider {
	if voice == "" {
		voice = "zh-CN-XiaoxiaoNeural"
	}
	if clientVersion == "" {
		clientVersion = edgeChromiumFullVersion
	}
	return &EdgeProvider{voice: voice, rate: "+0%", pitch: "+0Hz", clientVersion: clientVersion, safeDialer: safeDialer}
}

// edgeSecMSGEC 计算 Sec-MS-GEC 反滥用令牌（2024-11 起强制，缺失即 403，T2）。
// 算法与 edge-tts 7.2.8 的 drm.generate_sec_ms_gec 一致：
// 时间换算到 Windows FILETIME 纪元，向下取整到 5 分钟，换算为 100ns tick，
// 与 TrustedClientToken 拼接后取 SHA256 大写十六进制。
func edgeSecMSGEC(unixSec, skewSec int64) string {
	t := unixSec + winEpochSeconds + skewSec
	t -= t % 300
	ticks := t * 10_000_000
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d%s", ticks, edgeTTSToken)))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// edgeMajor 从完整版本号取主版本号（"143.0.3650.75" → "143"）。
func edgeMajor(full string) string {
	if i := strings.IndexByte(full, '.'); i > 0 {
		return full[:i]
	}
	return full
}

// edgeMuid 生成 32 位随机大写十六进制，充当 Cookie 里的 muid。
func edgeMuid() string {
	// uuid v4 取自 crypto/rand 且无错误返回路径；去掉连字符正好 32 位十六进制，
	// 比手写 rand.Read 再丢弃 error 更不易出错（HE-1 禁止静默丢弃返回值）。
	return strings.ToUpper(strings.ReplaceAll(uuid.New().String(), "-", ""))
}

// dial 建立 WebSocket 连接。返回的 *http.Response 在握手失败时可能非 nil，供调用方取状态码与 Date。
func (p *EdgeProvider) dial(ctx context.Context) (*websocket.Conn, *http.Response, error) {
	connID := strings.ReplaceAll(uuid.New().String(), "-", "")
	gec := edgeSecMSGEC(time.Now().Unix(), p.clockSkew.Load())
	wsURL := fmt.Sprintf("%s?TrustedClientToken=%s&ConnectionId=%s&Sec-MS-GEC=%s&Sec-MS-GEC-Version=1-%s",
		edgeTTSWSURL, edgeTTSToken, connID, gec, p.clientVersion)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	if p.safeDialer != nil {
		dialer.NetDialContext = p.safeDialer.DialContext // A-2：注入 SafeDialer，防 SSRF
	}
	major := edgeMajor(p.clientVersion)
	hdr := http.Header{}
	hdr.Set("User-Agent", fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36 Edg/%s.0.0.0", major, major))
	hdr.Set("Origin", edgeTTSOrigin)
	hdr.Set("Pragma", "no-cache")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Accept-Language", "en-US,en;q=0.9")
	hdr.Set("Cookie", "muid="+edgeMuid()+";")

	// 错误由唯一调用方 dialWithSkewRetry 统一包装并附 HTTP 状态码；此处保持原样以保留 resp。
	return dialer.DialContext(ctx, wsURL, hdr) //nolint:wrapcheck // 调用方 dialWithSkewRetry 统一 apperr.Wrap
}

// dialWithSkewRetry 握手；403 时按响应 Date 头校正时钟偏差后重试一次。
func (p *EdgeProvider) dialWithSkewRetry(ctx context.Context) (*websocket.Conn, error) {
	conn, resp, err := p.dial(ctx)
	if err == nil {
		return conn, nil
	}
	if resp != nil && resp.StatusCode == http.StatusForbidden {
		if serverTime, perr := http.ParseTime(resp.Header.Get("Date")); perr == nil {
			skew := serverTime.Unix() - time.Now().Unix()
			p.clockSkew.Store(skew)
			slog.Warn("edge-tts: 403，按服务器 Date 校正时钟偏差后重试", "skew_sec", skew)
			conn, resp, err = p.dial(ctx)
			if err == nil {
				return conn, nil
			}
		}
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("edge-tts: dial failed (HTTP %d)", status), err)
}

// Generate 调用 Edge TTS WebSocket 合成语音并返回 MP3 字节流。
func (p *EdgeProvider) Generate(ctx context.Context, text string) (Audio, error) {
	conn, err := p.dialWithSkewRetry(ctx)
	if err != nil {
		return Audio{}, err
	}
	defer conn.Close()

	// context 取消时通过设置读超时解除 ReadMessage 阻塞
	done := make(chan struct{})
	defer close(done)
	concurrent.SafeGo(ctx, "llm.tts.edge_cancel_watcher", func(gctx context.Context) {
		select {
		case <-gctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-done:
		}
	})

	if err := edgeSendRequests(conn, p, text); err != nil {
		return Audio{}, err
	}

	mp3, err := edgeReadAudio(ctx, conn)
	if err != nil {
		return Audio{}, err
	}
	return Audio{Data: mp3, MIME: MIMEMP3}, nil
}

// Close 实现 Provider 接口（EdgeProvider 无持久连接，空操作）。
func (p *EdgeProvider) Close() error { return nil }

// buildSSML 构造符合 Edge 免费端点的 SSML：只含 <voice><prosody>。
// 刻意不含 mstts:express-as——该端点会以 1007 "SSML is invalid" 拒绝（T4）。
func buildSSML(p *EdgeProvider, text string) string {
	lang := edgeVoiceLang(p.voice)
	escaped := edgeEscapeXML(text)
	return fmt.Sprintf(
		"<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='%s'>"+
			"<voice name='%s'><prosody pitch='%s' rate='%s' volume='+0%%'>%s</prosody></voice></speak>",
		lang, p.voice, p.pitch, p.rate, escaped)
}

// edgeSendRequests 向已建立的 WebSocket 连接发送 speech.config 和 SSML 两条消息。
func edgeSendRequests(conn *websocket.Conn, p *EdgeProvider, text string) error {
	ts := edgeTimestamp()
	reqID := strings.ReplaceAll(uuid.New().String(), "-", "")

	configMsg := fmt.Sprintf(
		"X-Timestamp:%s\r\nContent-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n"+
			`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":%q}}}}`,
		ts, edgeTTSOutputFmt)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(configMsg)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "edge-tts: write config failed", err)
	}

	ssml := buildSSML(p, text)
	ssmlMsg := fmt.Sprintf(
		"X-RequestId:%s\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:%s\r\nPath:ssml\r\n\r\n%s",
		reqID, ts, ssml)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(ssmlMsg)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "edge-tts: write ssml failed", err)
	}
	return nil
}

// edgeReadAudio 从 WebSocket 连接中读取所有音频帧，直到收到 "Path:turn.end"。
// 返回拼接后的 MP3 字节流。
func edgeReadAudio(ctx context.Context, conn *websocket.Conn) ([]byte, error) {
	var audioBuf bytes.Buffer
loop:
	for {
		if err := ctx.Err(); err != nil {
			return nil, err //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
		}
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				break loop
			}
			// 服务端关闭原因（如 1007 "Unsupported Edge output format" / "SSML is invalid"）
			// 必须进错误信息，否则只剩一句 read failed，无法定位协议层拒绝原因。
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				return nil, apperr.New(apperr.CodeInternal,
					fmt.Sprintf("edge-tts: server closed connection: code=%d reason=%q", ce.Code, ce.Text))
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

// edgeAppendAudioFrame 解析二进制帧并将音频数据追加到 buf。
// 帧格式：2字节 header 长度（big-endian）+ header 文本 + 音频字节。
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
