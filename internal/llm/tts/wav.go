package tts

import (
	"bytes"
	"encoding/binary"
	"math"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// encodeWAV 将 float32 PCM 数组（范围 [-1, 1]）转换为单声道 16-bit WAV 字节流。
// 超出范围的值截断到 int16 边界（不四舍五入）。
func encodeWAV(samples []float32, sampleRate int) ([]byte, error) {
	numSamples := len(samples)
	dataSize := numSamples * 2
	fileSize := 36 + dataSize

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	if err := binary.Write(&buf, binary.LittleEndian, int32(fileSize)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "encodeWAV", err)
	}
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	if err := binary.Write(&buf, binary.LittleEndian, int32(16)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "encodeWAV", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, int16(1)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "encodeWAV", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, int16(1)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "encodeWAV", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, int32(sampleRate)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "encodeWAV", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, int32(sampleRate*2)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "encodeWAV", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, int16(2)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "encodeWAV", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, int16(16)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "encodeWAV", err)
	}

	buf.WriteString("data")
	if err := binary.Write(&buf, binary.LittleEndian, int32(dataSize)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "encodeWAV", err)
	}

	for _, sample := range samples {
		// float32 [-1, 1] → int16 [-32768, 32767]，超限截断
		s := int16(math.Max(-32768, math.Min(32767, float64(sample)*32767.0)))
		if err := binary.Write(&buf, binary.LittleEndian, s); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "encodeWAV", err)
		}
	}

	return buf.Bytes(), nil
}
