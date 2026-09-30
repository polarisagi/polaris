package stt

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func createTestWAV(format uint16, channels uint16, sampleRate uint32, bitsPerSample uint16, rawData []byte, addJunkChunk bool) []byte {
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	riffLenPos := buf.Len()
	binary.Write(&buf, binary.LittleEndian, uint32(0)) // placeholder
	buf.WriteString("WAVE")

	if addJunkChunk {
		buf.WriteString("JUNK")
		junkData := []byte("odd!") // 4 bytes or 5 bytes (odd)
		binary.Write(&buf, binary.LittleEndian, uint32(len(junkData)))
		buf.Write(junkData)
		if len(junkData)%2 != 0 {
			buf.WriteByte(0) // pad
		}
	}

	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, format)
	binary.Write(&buf, binary.LittleEndian, channels)
	binary.Write(&buf, binary.LittleEndian, sampleRate)
	byteRate := sampleRate * uint32(channels) * uint32(bitsPerSample/8)
	binary.Write(&buf, binary.LittleEndian, byteRate)
	blockAlign := channels * (bitsPerSample / 8)
	binary.Write(&buf, binary.LittleEndian, blockAlign)
	binary.Write(&buf, binary.LittleEndian, bitsPerSample)

	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(len(rawData)))
	buf.Write(rawData)
	if len(rawData)%2 != 0 {
		buf.WriteByte(0)
	}

	totalLen := buf.Len() - 8
	out := buf.Bytes()
	binary.LittleEndian.PutUint32(out[riffLenPos:riffLenPos+4], uint32(totalLen))
	return out
}

func TestDecodeWAV_PCM16Mono(t *testing.T) {
	// Create 4 samples: 0.0, 0.5, -0.5, 1.0
	inputFloats := []float32{0.0, 0.5, -0.5, 0.9999}
	rawData := make([]byte, 0, len(inputFloats)*2)
	for _, f := range inputFloats {
		val := int16(f * 32767.0)
		buf := make([]byte, 2)
		binary.LittleEndian.PutUint16(buf, uint16(val))
		rawData = append(rawData, buf...)
	}

	wavBytes := createTestWAV(1, 1, 16000, 16, rawData, true)

	samples, sr, err := DecodeWAV(bytes.NewReader(wavBytes))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sr != 16000 {
		t.Errorf("expected sampleRate 16000, got %d", sr)
	}
	if len(samples) != len(inputFloats) {
		t.Fatalf("expected %d samples, got %d", len(inputFloats), len(samples))
	}

	for i := range samples {
		if math.Abs(float64(samples[i]-inputFloats[i])) > 0.01 {
			t.Errorf("sample %d: expected ~%f, got %f", i, inputFloats[i], samples[i])
		}
	}
}

func TestDecodeWAV_Float32Mono(t *testing.T) {
	inputFloats := []float32{0.1, -0.2, 0.8, -0.95}
	rawData := make([]byte, 0, len(inputFloats)*4)
	for _, f := range inputFloats {
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, math.Float32bits(f))
		rawData = append(rawData, buf...)
	}

	wavBytes := createTestWAV(3, 1, 24000, 32, rawData, false)

	samples, sr, err := DecodeWAV(bytes.NewReader(wavBytes))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sr != 24000 {
		t.Errorf("expected sampleRate 24000, got %d", sr)
	}
	if len(samples) != len(inputFloats) {
		t.Fatalf("expected %d samples, got %d", len(inputFloats), len(samples))
	}

	for i := range samples {
		if math.Abs(float64(samples[i]-inputFloats[i])) > 1e-6 {
			t.Errorf("sample %d: expected %f, got %f", i, inputFloats[i], samples[i])
		}
	}
}

func TestDecodeWAV_StereoDownmix(t *testing.T) {
	// Frame 0: Left=0.4, Right=0.6 -> Mono Avg=0.5
	// Frame 1: Left=-0.8, Right=0.2 -> Mono Avg=-0.3
	leftVals := []float32{0.4, -0.8}
	rightVals := []float32{0.6, 0.2}

	rawData := make([]byte, 0, len(leftVals)*4)
	for i := range leftVals {
		l := int16(leftVals[i] * 32767.0)
		r := int16(rightVals[i] * 32767.0)
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint16(buf[0:2], uint16(l))
		binary.LittleEndian.PutUint16(buf[2:4], uint16(r))
		rawData = append(rawData, buf...)
	}

	wavBytes := createTestWAV(1, 2, 16000, 16, rawData, false)

	samples, sr, err := DecodeWAV(bytes.NewReader(wavBytes))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sr != 16000 {
		t.Errorf("expected sampleRate 16000, got %d", sr)
	}
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}

	expected := []float32{0.5, -0.3}
	for i := range samples {
		if math.Abs(float64(samples[i]-expected[i])) > 0.01 {
			t.Errorf("sample %d: expected ~%f, got %f", i, expected[i], samples[i])
		}
	}
}

func TestDecodeWAV_InvalidInputs(t *testing.T) {
	// Empty reader
	_, _, err := DecodeWAV(bytes.NewReader(nil))
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for empty reader, got %v", err)
	}

	// Corrupted magic bytes
	_, _, err = DecodeWAV(bytes.NewReader([]byte("NOT_A_WAV_FILE_HEADER")))
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for bad header, got %v", err)
	}

	// Unsupported compression format (e.g. 6 = A-law)
	badFormatBytes := createTestWAV(6, 1, 16000, 16, []byte{1, 2, 3, 4}, false)
	_, _, err = DecodeWAV(bytes.NewReader(badFormatBytes))
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for format 6, got %v", err)
	}
}
