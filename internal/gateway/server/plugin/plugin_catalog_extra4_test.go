package plugin

import (
	"context"
	"testing"
)

func TestRegisterOneSkill(t *testing.T) {
	h := &PluginHandler{}
	h.registerOneSkill(context.Background(), "test", "test", "test", 0)
}
