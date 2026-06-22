package skill

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type mockLLMClient struct {
	ret string
	err error
}

func (m *mockLLMClient) Generate(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return m.ret, m.err
}

func TestNewSkillCreator(t *testing.T) {
	c := NewSkillCreator(nil, "base", nil)
	if c.baseDir != "base" {
		t.Errorf("bad base dir")
	}
}

func TestGenerateSkill_NilLLM(t *testing.T) {
	c := NewSkillCreator(nil, "base", nil)
	_, err := c.GenerateSkill(context.Background(), "intent")
	if err == nil {
		t.Errorf("expected nil LLM error")
	}
}

func TestGenerateSkill_LLMError(t *testing.T) {
	c := NewSkillCreator(&mockLLMClient{err: apperr.New(apperr.CodeInternal, "llm err")}, "base", nil)
	_, err := c.GenerateSkill(context.Background(), "intent")
	if err == nil {
		t.Errorf("expected LLM error")
	}
}

func TestGenerateSkill_ParseError(t *testing.T) {
	c := NewSkillCreator(&mockLLMClient{ret: "invalid json"}, "base", nil)
	_, err := c.GenerateSkill(context.Background(), "intent")
	if err == nil {
		t.Errorf("expected parse error")
	}
}

func TestGenerateSkill_MissingName(t *testing.T) {
	c := NewSkillCreator(&mockLLMClient{ret: `{"description":"test"}`}, "base", nil)
	_, err := c.GenerateSkill(context.Background(), "intent")
	if err == nil {
		t.Errorf("expected missing name error")
	}
}

func TestGenerateSkill_Success(t *testing.T) {
	c := NewSkillCreator(&mockLLMClient{ret: "```json\n" + `{"name":"test-skill","description":"test","instructions":"do test"}` + "\n```"}, t.TempDir(), nil)

	_, err := c.GenerateSkill(context.Background(), "intent")

	// Expect failure because installMgr is nil
	if err == nil {
		t.Errorf("expected security manager missing error")
	}
}

func TestExtractJSON(t *testing.T) {
	s := extractJSON("some text\n```json\n{\"a\":1}\n```\nmore text")
	if s != "{\"a\":1}" {
		t.Errorf("failed to extract json: %q", s)
	}

	s = extractJSON("{\"a\":1}")
	if s != "{\"a\":1}" {
		t.Errorf("failed to extract json directly")
	}
}
