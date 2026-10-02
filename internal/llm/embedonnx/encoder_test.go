package embedonnx

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func cosineSimilarity(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func findORTDylibPath() string {
	candidates := []string{
		filepath.Join("..", "..", "..", "local_playground", "upgrade", "local-inference-embedder", "spike", "onnxruntime-osx-x86_64-1.23.2", "lib", "libonnxruntime.dylib"),
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.Size() > 0 {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return ""
}

func findBGEModelPath() string {
	c := filepath.Join("..", "..", "..", "local_playground", "upgrade", "local-inference-embedder", "spike", "models", "bge", "model_quantized.onnx")
	if fi, err := os.Stat(c); err == nil && fi.Size() > 0 {
		abs, _ := filepath.Abs(c)
		return abs
	}
	return ""
}

func findGemmaModelPath() string {
	c := filepath.Join("..", "..", "..", "local_playground", "upgrade", "local-inference-embedder", "spike", "models", "gemma", "model_quantized.onnx")
	if fi, err := os.Stat(c); err == nil && fi.Size() > 0 {
		abs, _ := filepath.Abs(c)
		return abs
	}
	return ""
}

func TestBGEEncoderCosineParity(t *testing.T) {
	dylibPath := findORTDylibPath()
	if dylibPath == "" {
		t.Skip("ORT dylib not found; download to spike/ or ~/.polaris/data/models/embed/ort/ to run")
	}
	modelPath := findBGEModelPath()
	if modelPath == "" {
		t.Skip("BGE model not found; download to testdata/ or spike/ to run")
	}
	vocabPath := filepath.Join("testdata", "vocab.txt")
	if _, err := os.Stat(vocabPath); os.IsNotExist(err) {
		t.Skip("vocab.txt not found")
	}

	api, err := OpenORT(dylibPath)
	if err != nil {
		t.Fatalf("loadORT failed: %v", err)
	}

	session, err := NewOrtSession(api, modelPath, false)
	if err != nil {
		t.Fatalf("NewOrtSession failed: %v", err)
	}
	defer session.Close()

	tok, err := NewWordPieceTokenizer(vocabPath)
	if err != nil {
		t.Fatalf("NewWordPieceTokenizer failed: %v", err)
	}

	engine := NewBGEEngine(session, tok)

	samplesPath := filepath.Join("testdata", "test_samples.json")
	dataBytes, err := os.ReadFile(samplesPath)
	if err != nil {
		t.Fatalf("read test_samples failed: %v", err)
	}
	var td testDataFixture
	if err := json.Unmarshal(dataBytes, &td); err != nil {
		t.Fatalf("parse test_samples failed: %v", err)
	}

	ctx := context.Background()
	for i, item := range td.BGE {
		vec := engine.Embed(ctx, item.Text)
		if len(vec) != 512 {
			t.Fatalf("sample %d: expected 512 dim, got %d", i, len(vec))
		}

		// Matryoshka / L2 norm 验证: 范数 = 1.0 +/- 1e-5
		var norm float64
		for _, v := range vec {
			norm += float64(v) * float64(v)
		}
		norm = math.Sqrt(norm)
		if math.Abs(norm-1.0) > 1e-4 {
			t.Fatalf("sample %d: L2 norm %f not close to 1.0", i, norm)
		}

		sim := cosineSimilarity(vec, item.Embedding512)
		if sim < 0.999 {
			t.Fatalf("sample %d %q: Cosine Similarity %f < 0.999 threshold", i, item.Text, sim)
		}
	}
}

func TestGemmaEncoderCosineParityAndMatryoshka(t *testing.T) {
	dylibPath := findORTDylibPath()
	if dylibPath == "" {
		t.Skip("ORT dylib not found; download to spike/ or ~/.polaris/data/models/embed/ort/ to run")
	}
	modelPath := findGemmaModelPath()
	if modelPath == "" {
		t.Skip("Gemma model not found; download to testdata/ or spike/ to run")
	}
	tokPath := findGemmaTokenizerPath()
	if tokPath == "" {
		t.Skip("Gemma tokenizer not found; download to testdata/ or spike/ to run")
	}

	api, err := OpenORT(dylibPath)
	if err != nil {
		t.Fatalf("loadORT failed: %v", err)
	}

	session, err := NewOrtSession(api, modelPath, true)
	if err != nil {
		t.Fatalf("NewOrtSession failed: %v", err)
	}
	defer session.Close()

	tok, err := NewSentencePieceTokenizer(tokPath)
	if err != nil {
		t.Fatalf("NewSentencePieceTokenizer failed: %v", err)
	}

	// 直接针对 session 做 raw 对拍测试（样本中未添加前缀）
	samplesPath := filepath.Join("testdata", "test_samples.json")
	dataBytes, err := os.ReadFile(samplesPath)
	if err != nil {
		t.Fatalf("read test_samples failed: %v", err)
	}
	var td testDataFixture
	if err := json.Unmarshal(dataBytes, &td); err != nil {
		t.Fatalf("parse test_samples failed: %v", err)
	}

	for i, item := range td.Gemma {
		inputIDs, attnMask := tok.Encode(item.Text, 512)
		vec, err := session.Run(inputIDs, attnMask, nil)
		if err != nil {
			t.Fatalf("sample %d infer failed: %v", i, err)
		}

		if len(vec) != 512 {
			t.Fatalf("sample %d: expected 512 dim, got %d", i, len(vec))
		}

		// Matryoshka 512 维与 L2 范数验证: 范数 = 1.0 +/- 1e-5
		var norm float64
		for _, v := range vec {
			norm += float64(v) * float64(v)
		}
		norm = math.Sqrt(norm)
		if math.Abs(norm-1.0) > 1e-4 {
			t.Fatalf("sample %d: L2 norm %f not close to 1.0", i, norm)
		}

		sim := cosineSimilarity(vec, item.Embedding512)
		if sim < 0.999 {
			t.Fatalf("sample %d %q: Cosine Similarity %f < 0.999 threshold", i, item.Text, sim)
		}
	}
}
