package security

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestSecurityFacade_IsAuthorized_NilPolicy(t *testing.T) {
	facade := NewSecurityFacade(nil, nil, nil)

	allowed, err := facade.IsAuthorized(context.Background(), "user", "action", "resource", nil)

	if allowed {
		t.Errorf("expected allowed=false, got true")
	}
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !apperr.IsCode(err, apperr.CodeInternal) {
		t.Errorf("expected CodeInternal, got %v", apperr.CodeOf(err))
	}
}
