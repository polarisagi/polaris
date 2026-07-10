package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestDoStreamInfer_StreamError(t *testing.T) {
	a := &Agent{} // minimal mock for testing doStreamInfer

	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{
		Type:    types.StreamError,
		Content: "simulated stream error",
	}
	close(ch)

	_, err := a.doStreamInfer(context.Background(), ch)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !apperr.IsCode(err, apperr.CodeProviderExhausted) {
		t.Errorf("expected error code %s, got err: %v", apperr.CodeProviderExhausted, err)
	}
}
