package orchestrator

import (
	"os"
	"strconv"
	"time"
)

// Pipeline roles. v1 is a fixed software-delivery pipeline; each hop is gated by a
// human approval (see routes.go). nextRole defines the handoff order.
const (
	RolePM       = "product-manager"
	RoleEngineer = "engineer"
	RoleReviewer = "reviewer"
)

// pipeline is the ordered role chain. Approving a role's task hands the work off
// to the next role; the last role's approval ends the chain (-> done).
var pipeline = []string{RolePM, RoleEngineer, RoleReviewer}

// nextRole returns the role that follows `role` in the pipeline, and whether one
// exists. Roles outside the pipeline have no successor (they just finish).
func nextRole(role string) (string, bool) {
	for i, r := range pipeline {
		if r == role && i+1 < len(pipeline) {
			return pipeline[i+1], true
		}
	}
	return "", false
}

// config is the orchestrator's runtime knobs, read once at startup. All via env so
// you can tune cadence and (critically) cap spend without a redeploy of behavior.
type config struct {
	enabled          bool
	interval         time.Duration // tick cadence
	maxTokens        int           // per-call output cap
	dailyTokenBudget int           // 0 = unlimited; halts dispatch once reached
	callTimeout      time.Duration
	provider         string // default provider for seeded agents
	model            string // default model ("" = provider default)
}

func configFromEnv() config {
	return config{
		enabled:          envBool("ORCH_ENABLED", true),
		interval:         envDuration("ORCH_INTERVAL", 15*time.Second),
		maxTokens:        envInt("ORCH_MAX_TOKENS", 2000),
		dailyTokenBudget: envInt("ORCH_DAILY_TOKEN_BUDGET", 200_000),
		callTimeout:      envDuration("ORCH_CALL_TIMEOUT", 120*time.Second),
		provider:         envStr("ORCH_PROVIDER", "anthropic"),
		model:            envStr("ORCH_MODEL", ""),
	}
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
