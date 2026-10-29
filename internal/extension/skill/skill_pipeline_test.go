package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type mockExecutor struct {
	resp []byte
	err  error
}

func (m *mockExecutor) ExecuteTest(ctx context.Context, scriptBytes []byte, input []byte) ([]byte, error) {
	return m.resp, m.err
}

func TestSkillValidationPipeline(t *testing.T) {
	exec := &mockExecutor{resp: []byte("pass")}
	pipe := NewSkillValidationPipeline([]byte("secret-key"), exec)

	// Step 0: Taint-Check (implicitly through Validate or explicit)
	if err := pipe.taintChecker.Check(0); err != nil {
		t.Fatalf("Taint check failed: %v", err)
	}

	// Step 1: Analyze
	code := []byte(`
function hello() {
	return "pass"
}
	`)

	// Add test cases
	pipe.scriptTester.AddTestCase("test_pass", []byte("input"), []byte("pass"))

	// Validate (Run -> Assess -> Sign)
	res, err := pipe.Validate(code, 0)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if !res.Passed {
		t.Errorf("expected validate to pass")
	}

}

func TestVerifySign(t *testing.T) {
	pipe := NewSkillValidationPipeline([]byte("secret"), &mockExecutor{})
	code := []byte("code")

	sig, err := pipe.signer.Sign(code)
	if err != nil {
		t.Fatalf("sign error: %v", err)
	}

	if !pipe.signer.Verify(code, sig) {
		t.Errorf("verify failed")
	}

	if pipe.signer.Verify(code, "invalid") {
		t.Errorf("expected verify to fail on invalid signature")
	}
}
func TestSkillValidationPipeline_MaxCodeSize(t *testing.T) {
	pipe := NewSkillValidationPipeline([]byte("secret"), &mockExecutor{}, WithMaxCodeSize(50))
	code := []byte(strings.Repeat("a", 51))
	_, err := pipe.Validate(code, 0)
	if err == nil {
		t.Fatal("expected error for exceeding max code size, got nil")
	}
	appErr, ok := err.(*apperr.Error)
	if !ok || appErr.Code != apperr.CodeInvalidInput {
		t.Fatalf("expected CodeInvalidInput error, got %v", err)
	}
}
