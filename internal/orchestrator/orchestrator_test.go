package orchestrator

import (
	"testing"
	"time"
)

func TestNextRole(t *testing.T) {
	cases := []struct {
		role     string
		wantNext string
		wantOK   bool
	}{
		{RolePM, RoleEngineer, true},
		{RoleEngineer, RoleReviewer, true},
		{RoleReviewer, "", false}, // last in pipeline -> no handoff
		{"marketing", "", false},  // not in pipeline
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := nextRole(c.role)
		if got != c.wantNext || ok != c.wantOK {
			t.Errorf("nextRole(%q) = (%q, %v), want (%q, %v)", c.role, got, ok, c.wantNext, c.wantOK)
		}
	}
}

func TestBuildPrompt(t *testing.T) {
	if got := buildPrompt("Add login", "Use OAuth"); got != "Task: Add login\n\nUse OAuth" {
		t.Errorf("with description = %q", got)
	}
	if got := buildPrompt("Add login", ""); got != "Task: Add login" {
		t.Errorf("without description = %q", got)
	}
}

func TestOrElse(t *testing.T) {
	if orElse("a", "b") != "a" {
		t.Error("non-empty should win")
	}
	if orElse("", "b") != "b" {
		t.Error("empty should fall back")
	}
}

func TestConfigDefaults(t *testing.T) {
	// Ensure a clean env for the knobs we assert.
	for _, k := range []string{"ORCH_ENABLED", "ORCH_INTERVAL", "ORCH_MAX_TOKENS", "ORCH_DAILY_TOKEN_BUDGET", "ORCH_PROVIDER"} {
		t.Setenv(k, "")
	}
	c := configFromEnv()
	if !c.enabled {
		t.Error("default enabled should be true")
	}
	if c.interval != 15*time.Second {
		t.Errorf("default interval = %v, want 15s", c.interval)
	}
	if c.maxTokens != 2000 {
		t.Errorf("default maxTokens = %d, want 2000", c.maxTokens)
	}
	if c.dailyTokenBudget != 200_000 {
		t.Errorf("default budget = %d, want 200000", c.dailyTokenBudget)
	}
	if c.provider != "anthropic" {
		t.Errorf("default provider = %q, want anthropic", c.provider)
	}
}

func TestConfigOverrides(t *testing.T) {
	t.Setenv("ORCH_ENABLED", "false")
	t.Setenv("ORCH_INTERVAL", "5s")
	t.Setenv("ORCH_DAILY_TOKEN_BUDGET", "0")
	t.Setenv("ORCH_PROVIDER", "openai")
	c := configFromEnv()
	if c.enabled {
		t.Error("enabled should be false")
	}
	if c.interval != 5*time.Second {
		t.Errorf("interval = %v, want 5s", c.interval)
	}
	if c.dailyTokenBudget != 0 {
		t.Errorf("budget = %d, want 0 (unlimited)", c.dailyTokenBudget)
	}
	if c.provider != "openai" {
		t.Errorf("provider = %q, want openai", c.provider)
	}
}
