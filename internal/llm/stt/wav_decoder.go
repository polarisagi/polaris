package stt

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type wavHeaderInfo struct {
	audioFormat   uint16
	numChannels   uint16
	sampleRate    uint32
	bitsPerSample uint16
}

// parseRIFF reads and validates the 12-byte RIFF WAVE header.
func parseRIFF(r io.Reader) error {
	var riffHeader [12]byte
	if _, err := io.ReadFull(r, riffHeader[:]); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "wav: failed to read RIFF header", err)
	}
	if string(riffHeader[0:4]) != "RIFF" || string(riffHeader[8:12]) != "WAVE" {
		return apperr.New(apperr.CodeInvalidInput, "wav: invalid format, expected RIFF/WAVE")
	}
	return nil
}

// readFmtChunk parses the "fmt " subchunk payload.
func readFmtChunk(r io.Reader, chunkSize uint32) (wavHeaderInfo, error) {
	if chunkSize < 16 {
		return wavHeaderInfo{}, apperr.New(apperr.CodeInvalidInput, "wav: fmt chunk too short")
	}
	var fmtBuf [16]byte
	if _, err := io.ReadFull(r, fmtBuf[:]); err != nil {
		return wavHeaderInfo{}, apperr.Wrap(apperr.CodeInvalidInput, "wav: failed to read fmt body", err)
	}
	info := wavHeaderInfo{
		audioFormat:   binary.LittleEndian.Uint16(fmtBuf[0:2]),
		numChannels:   binary.LittleEndian.Uint16(fmtBuf[2:4]),
		sampleRate:    binary.LittleEndian.Uint32(fmtBuf[4:8]),
		bitsPerSample: binary.LittleEndian.Uint16(fmtBuf[14:16]),
	}
	remain := int64(chunkSize - 16)
	if remain > 0 {
		if _, err := io.CopyN(io.Discard, r, remain); err != nil {
			return wavHeaderInfo{}, apperr.Wrap(apperr.CodeInvalidInput, "wav: failed to skip fmt extra bytes", err)
		}
	}
	return info, nil
}

// readDataChunk parses the "data" subchunk payload.
func readDataChunk(r io.Reader, chunkSize uint32, fmtFound bool) ([]byte, error) {
	if !fmtFound {
		return nil, apperr.New(apperr.CodeInvalidInput, "wav: data chunk preceded fmt chunk")
	}
	dataBytes := make([]byte, chunkSize)
	if _, err := io.ReadFull(r, dataBytes); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "wav: failed to read audio data", err)
	}
	return dataBytes, nil
}

// skipPadding discards the single pad byte if the chunk size is odd.
func skipPadding(r io.Reader, chunkSize uint32) error {
	if chunkSize%2 != 0 {
		if _, err := io.CopyN(io.Discard, r, 1); err != nil {
			return apperr.Wrap(apperr.CodeInvalidInput, "wav: failed to skip pad byte", err)
		}
	}
	return nil
}

// readWAVChunks scans chunks until fmt and data are found.
func readWAVChunks(r io.Reader) (wavHeaderInfo, []byte, error) {
	var (
		info      wavHeaderInfo
		fmtFound  bool
		dataBytes []byte
	)

	for len(dataBytes) == 0 {
		var chunkHeader [8]byte
		if _, err := io.ReadFull(r, chunkHeader[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return info, nil, apperr.Wrap(apperr.CodeInvalidInput, "wav: failed to read chunk header", err)
		}

		chunkID := string(chunkHeader[0:4])
		chunkSize := binary.LittleEndian.Uint32(chunkHeader[4:8])

		switch chunkID {
		case "fmt ":
			parsed, err := readFmtChunk(r, chunkSize)
			if err != nil {
				return info, nil, err
			}
			info = parsed
			fmtFound = true

		case "data":
			bytes, err := readDataChunk(r, chunkSize, fmtFound)
			if err != nil {
				return info, nil, err
			}
			dataBytes = bytes

		default:
			if _, err := io.CopyN(io.Discard, r, int64(chunkSize)); err != nil {
				return info, nil, apperr.Wrap(apperr.CodeInvalidInput, "wav: failed to skip chunk "+chunkID, err)
			}
		}

		if err := skipPadding(r, chunkSize); err != nil {
			return info, nil, err
		}
	}

	if !fmtFound || len(dataBytes) == 0 {
		return info, nil, apperr.New(apperr.CodeInvalidInput, "wav: missing fmt or data chunk")
	}
	return info, dataBytes, nil
}

// decodePCMSample parses a single PCM integer sample of 8, 16, 24, or 32 bits.
func decodePCMSample(bitsPerSample uint16, b []byte) (float32, error) {
	switch bitsPerSample {
	case 8:
		return (float32(b[0]) - 128.0) / 128.0, nil
	case 16:
		raw := int16(binary.LittleEndian.Uint16(b[0:2]))
		return float32(raw) / 32768.0, nil
	case 24:
		b0 := uint32(b[0])
		b1 := uint32(b[1])
		b2 := uint32(b[2])
		rawU := b0 | (b1 << 8) | (b2 << 16)
		if rawU&0x800000 != 0 {
			rawU |= 0xFF000000
		}
		return float32(int32(rawU)) / 8388608.0, nil
	case 32:
		raw := int32(binary.LittleEndian.Uint32(b[0:4]))
		return float32(raw) / 2147483648.0, nil
	default:
		return 0, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("wav: unsupported PCM bit depth %d", bitsPerSample))
	}
}

// decodeSingleSample converts raw bytes of one sample into float32 [-1.0, 1.0].
func decodeSingleSample(audioFormat uint16, bitsPerSample uint16, b []byte) (float32, error) {
	switch audioFormat {
	case 1: // PCM
		return decodePCMSample(bitsPerSample, b)
	case 3: // IEEE Float
		if bitsPerSample != 32 {
			return 0, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("wav: unsupported float bit depth %d", bitsPerSample))
		}
		bits := binary.LittleEndian.Uint32(b[0:4])
		return math.Float32frombits(bits), nil
	default:
		return 0, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("wav: unsupported audio format code %d", audioFormat))
	}
}

// decodeFrames processes all frames, downmixing multi-channel audio to mono.
func decodeFrames(info wavHeaderInfo, dataBytes []byte) ([]float32, error) {
	if info.numChannels == 0 {
		return nil, apperr.New(apperr.CodeInvalidInput, "wav: invalid channel count 0")
	}

	bytesPerSample := int(info.bitsPerSample / 8)
	if bytesPerSample == 0 {
		return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("wav: unsupported bit depth %d", info.bitsPerSample))
	}

	frameSize := int(info.numChannels) * bytesPerSample
	numFrames := len(dataBytes) / frameSize
	if numFrames == 0 {
		return []float32{}, nil
	}

	samples := make([]float32, numFrames)
	numChF := float32(info.numChannels)

	for f := 0; f < numFrames; f++ {
		frameOffset := f * frameSize
		var frameSum float32

		for ch := 0; ch < int(info.numChannels); ch++ {
			sampleOffset := frameOffset + ch*bytesPerSample
			val, err := decodeSingleSample(info.audioFormat, info.bitsPerSample, dataBytes[sampleOffset:sampleOffset+bytesPerSample])
			if err != nil {
				return nil, err
			}
			frameSum += val
		}

		avg := frameSum / numChF
		if avg > 1.0 {
			avg = 1.0
		} else if avg < -1.0 {
			avg = -1.0
		}
		samples[f] = avg
	}

	return samples, nil
}

// DecodeWAV 从 WAV 数据流中解析并解码出单声道 float32 PCM 样本数组（归一化至 [-1.0, 1.0]）及采样率。
// 支持格式：
//   - 格式 1 (PCM): 8-bit 无符号、16-bit 有符号、24-bit 有符号、32-bit 有符号整数
//   - 格式 3 (IEEE Float): 32-bit 浮点数
//   - 单声道与多声道（多声道将自动平均下混为单声道，满足 ASR 引擎输入要求）
func DecodeWAV(r io.Reader) ([]float32, int, error) {
	if err := parseRIFF(r); err != nil {
		return nil, 0, err
	}

	info, dataBytes, err := readWAVChunks(r)
	if err != nil {
		return nil, 0, err
	}

	samples, err := decodeFrames(info, dataBytes)
	if err != nil {
		return nil, 0, err
	}

	return samples, int(info.sampleRate), nil
}
