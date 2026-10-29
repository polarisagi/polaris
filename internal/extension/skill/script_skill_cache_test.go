package skill

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestNewScriptSkillCache(t *testing.T) {
	c := NewScriptSkillCache(nil, 1, 1, 0) // bronzeMax 0 defaults to 32
	if c.bronzeMaxSize != 32 {
		t.Errorf("expected default bronzeMax 32")
	}
}

func mockSpawnFn(ctx context.Context, skillID string) (*ProcessHandle, error) {
	if skillID == "fail_spawn" {
		return nil, apperr.New(apperr.CodeInternal, "failed spawn")
	}
	return &ProcessHandle{SkillID: skillID, Closer: func() {}}, nil
}

func TestScriptSkillCache_GetOrSpawn(t *testing.T) {
	c := NewScriptSkillCache(mockSpawnFn, 1, 1, 1)

	// fail spawn
	_, err := c.GetOrSpawn(context.Background(), "fail_spawn")
	if err == nil {
		t.Errorf("expected spawn error")
	}

	// success spawn (bronze)
	h, err := c.GetOrSpawn(context.Background(), "s1")
	if err != nil || h == nil {
		t.Fatalf("expected handle")
	}

	// second fetch gets from bronze
	h2, _ := c.GetOrSpawn(context.Background(), "s1")
	if h != h2 {
		t.Errorf("expected same handle")
	}

	// TTL eviction
	c.mu.Lock()
	c.bronzeCache["s1"].expiresAt = time.Now().Add(-1 * time.Hour)
	c.mu.Unlock()

	h3, _ := c.GetOrSpawn(context.Background(), "s1")
	if h == h3 {
		t.Errorf("expected new handle after eviction")
	}
}

func TestScriptSkillCache_PromoteOrCache(t *testing.T) {
	c := NewScriptSkillCache(mockSpawnFn, 1, 1, 1)

	c.GetOrSpawn(context.Background(), "s1")

	// Promote to silver
	c.PromoteOrCache(SkillStats{SkillID: "s1", SuccessRate: 0.8})

	c.mu.Lock()
	_, inSilver := c.silverCache["s1"]
	_, inBronze := c.bronzeCache["s1"]
	c.mu.Unlock()

	if !inSilver || inBronze {
		t.Errorf("expected in silver, removed from bronze")
	}

	// Promote to gold
	c.PromoteOrCache(SkillStats{SkillID: "s1", SuccessRate: 0.95, TotalUsage: 100})

	c.mu.Lock()
	_, inGold := c.goldCache["s1"]
	_, inSilver = c.silverCache["s1"]
	c.mu.Unlock()

	if !inGold || inSilver {
		t.Errorf("expected in gold, removed from silver")
	}

	// Try promote unmanaged handle
	c.PromoteOrCache(SkillStats{SkillID: "unknown", SuccessRate: 1.0, TotalUsage: 100})

	c.mu.Lock()
	_, inGold = c.goldCache["unknown"]
	c.mu.Unlock()
	if inGold {
		t.Errorf("should not promote unknown handle")
	}
}

func TestScriptSkillCache_WarmGold(t *testing.T) {
	c := NewScriptSkillCache(mockSpawnFn, 10, 10, 10)
	c.WarmGold(context.Background(), []string{"g1", "g2"})

	time.Sleep(100 * time.Millisecond)

	c.mu.Lock()
	inBronze1 := c.bronzeCache["g1"] != nil
	inBronze2 := c.bronzeCache["g2"] != nil
	c.mu.Unlock()

	if !inBronze1 || !inBronze2 {
		t.Errorf("expected warm gold to populate bronze initially")
	}
}

func TestScriptSkillCache_Evict(t *testing.T) {
	c := NewScriptSkillCache(mockSpawnFn, 1, 1, 1)
	c.GetOrSpawn(context.Background(), "s1")

	c.Evict("s1")

	c.mu.Lock()
	b := c.bronzeCache["s1"]
	c.mu.Unlock()
	if b != nil {
		t.Errorf("expected eviction")
	}
}

func TestScriptSkillCache_LRU(t *testing.T) {
	c := NewScriptSkillCache(mockSpawnFn, 1, 1, 2)
	c.GetOrSpawn(context.Background(), "b1")
	c.GetOrSpawn(context.Background(), "b2")

	c.mu.Lock()
	if len(c.bronzeCache) != 2 {
		t.Errorf("expected 2 bronze items")
	}
	c.mu.Unlock()

	// Add b3, b1 should be evicted
	c.GetOrSpawn(context.Background(), "b3")

	c.mu.Lock()
	if c.bronzeCache["b1"] != nil {
		t.Errorf("expected b1 evicted")
	}
	if len(c.bronzeCache) != 2 {
		t.Errorf("expected 2 bronze items")
	}
	c.mu.Unlock()
}
