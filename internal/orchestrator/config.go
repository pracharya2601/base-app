package orchestrator

import (
	"os"
	"strconv"
	"time"

	"github.com/pocketbase/pocketbase/core"
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
	autoApprove      bool          // autopilot: auto-approve drafts + hand off (no human gate)
	interval         time.Duration // tick cadence
	maxTokens        int           // per-call output cap
	maxToolSteps     int           // max generate↔tool round-trips for tool-enabled tasks
	maxRevisions     int           // max rework passes per task after its first draft (0 = no rework)
	dailyTokenBudget int           // 0 = unlimited; halts dispatch once reached
	callTimeout      time.Duration
	provider         string // default provider for seeded agents
	model            string // default model ("" = provider default)
}

func configFromEnv() config {
	return config{
		enabled:          envBool("ORCH_ENABLED", true),
		autoApprove:      envBool("ORCH_AUTO_APPROVE", false), // default OFF — autonomous spend is opt-in
		interval:         envDuration("ORCH_INTERVAL", 15*time.Second),
		maxTokens:        envInt("ORCH_MAX_TOKENS", 2000),
		maxToolSteps:     envInt("ORCH_MAX_TOOL_STEPS", 5),
		maxRevisions:     envInt("ORCH_MAX_REVISIONS", 3),
		dailyTokenBudget: envInt("ORCH_DAILY_TOKEN_BUDGET", 200_000),
		callTimeout:      envDuration("ORCH_CALL_TIMEOUT", 120*time.Second),
		provider:         envStr("ORCH_PROVIDER", "anthropic"),
		model:            envStr("ORCH_MODEL", ""),
	}
}

// loadOrchConfig returns the EFFECTIVE config for a tenant: the ORCH_* env
// defaults overlaid with any per-tenant _orchConfigs row. This is what makes the
// company DB-configurable at runtime (no redeploy) — the tick calls it every tick.
//
// Zero-vs-unset rules (the row's fields default to zero):
//   - numbers/strings overlay only when SET (>0 / non-empty), so 0 means "use env";
//   - `autopilot` (a bool, where 0==false is a real value) is AUTHORITATIVE once a
//     row exists — its safe default is false, and the autopilot/config endpoints are
//     what create the row, so this reads back exactly what was toggled;
//   - `enabled` stays ENV-ONLY here — turning the whole loop off is an ops decision,
//     not a per-tenant runtime toggle, and false-vs-unset can't be told apart.
func loadOrchConfig(app core.App, ownerID string) config {
	cfg := configFromEnv()
	// FindFirstRecordByData (raw field match) handles owner=="" (the system tenant);
	// the PB filter-string parser does not reliably match an empty-string param.
	rec, err := app.FindFirstRecordByData(configCollection, "owner", ownerID)
	if err != nil || rec == nil {
		return cfg
	}
	cfg.autoApprove = rec.GetBool("autopilot")
	if v := rec.GetInt("intervalSeconds"); v > 0 {
		cfg.interval = time.Duration(v) * time.Second
	}
	if v := rec.GetInt("maxTokens"); v > 0 {
		cfg.maxTokens = v
	}
	if v := rec.GetInt("maxRevisions"); v > 0 {
		cfg.maxRevisions = v
	}
	if v := rec.GetInt("dailyTokenBudget"); v > 0 {
		cfg.dailyTokenBudget = v
	}
	if v := rec.GetString("provider"); v != "" {
		cfg.provider = v
	}
	if v := rec.GetString("model"); v != "" {
		cfg.model = v
	}
	return cfg
}

// upsertOrchConfig creates or updates a tenant's _orchConfigs row, applying the
// given field setters. The owner-unique index keeps it one row per tenant.
func upsertOrchConfig(app core.App, ownerID string, apply func(*core.Record)) (*core.Record, error) {
	rec, err := app.FindFirstRecordByData(configCollection, "owner", ownerID)
	if err != nil || rec == nil {
		col, cerr := app.FindCollectionByNameOrId(configCollection)
		if cerr != nil {
			return nil, cerr
		}
		rec = core.NewRecord(col)
		rec.Set("owner", ownerID)
	}
	apply(rec)
	if err := app.Save(rec); err != nil {
		return nil, err
	}
	return rec, nil
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
