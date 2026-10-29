package skill

import (
	"testing"
)

func FuzzSkillValidationPipeline(f *testing.F) {
	// Add corpus
	f.Add([]byte("print('hello world')"), "skill:test_skill")
	f.Add([]byte("import os\nos.system('ls')"), "skill:test_unsafe")

	f.Fuzz(func(t *testing.T, input []byte, name string) {
		_ = name // ignore unused name for now

		pipeline := NewSkillValidationPipeline(nil, nil, WithMaxCodeSize(16384))

		// Simply validate doesn't panic. The pipeline handles many edge cases.
		_, _ = pipeline.Validate(input, 1)
	})
}
