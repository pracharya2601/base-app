package ai

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// Rate limiting + per-user token quota for the AI proxy, enforced BEFORE the
// upstream provider call (so a blocked request costs nothing). Two independent
// gates, both keyed by the authenticated identity (e.Auth.Id):
//
//   - requests/min : in-memory sliding window — fast, and closes the
//     concurrent-burst hole that a "count _aiUsage rows" check would leave
//     (rows are written only AFTER a call finishes).
//   - tokens/day   : summed from _aiUsage.totalTokens — the real cost cap;
//     persistent, so it survives restarts.
//
// Superusers are EXEMPT — and since an API key acts AS the minting superuser on
// the AI routes, trusted service-key traffic is exempt too. Limits target
// end-user JWTs (the actual abuse vector); keys are revocable. Config via env,
// read once at startup; 0 disables a gate.
//
//   AI_RATE_LIMIT_PER_MIN  (default 60)  max AI requests/min per user
//   AI_TOKEN_QUOTA_PER_DAY (default 0)   max tokens/24h per user (0 = unlimited)

type aiLimits struct {
	ratePerMin   int
	tokensPerDay int
}

// aiActiveLimits is set once at startup (registerAIRoutes) and read by the
// generate/stream/image handlers.
var aiActiveLimits aiLimits

func aiLimitsFromEnv() aiLimits {
	return aiLimits{
		ratePerMin:   envInt("AI_RATE_LIMIT_PER_MIN", 60),
		tokensPerDay: envInt("AI_TOKEN_QUOTA_PER_DAY", 0),
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// slidingWindow is a concurrency-safe per-identity request-timestamp log.
type slidingWindow struct {
	mu   sync.Mutex
	hits map[string][]int64 // identity -> unix-nano timestamps within the window
}

func newSlidingWindow() *slidingWindow { return &slidingWindow{hits: map[string][]int64{}} }

var aiRateWindow = newSlidingWindow()

// allow prunes old timestamps, then records and admits a hit if the identity is
// under `limit` over the trailing `window`. Returns whether admitted and the
// resulting count in the window.
func (s *slidingWindow) allow(id string, limit int, window time.Duration) (bool, int) {
	cutoff := time.Now().UnixNano() - int64(window)
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := pruneInPlace(s.hits[id], cutoff)
	if len(kept) >= limit {
		s.hits[id] = kept
		return false, len(kept)
	}
	kept = append(kept, time.Now().UnixNano())
	s.hits[id] = kept
	return true, len(kept)
}

// count returns the current window size for an identity WITHOUT recording a hit
// (used by the /limits endpoint). Doubles as opportunistic cleanup of stale keys.
func (s *slidingWindow) count(id string, window time.Duration) int {
	cutoff := time.Now().UnixNano() - int64(window)
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := pruneInPlace(s.hits[id], cutoff)
	if len(kept) == 0 {
		delete(s.hits, id)
		return 0
	}
	s.hits[id] = kept
	return len(kept)
}

func pruneInPlace(ts []int64, cutoff int64) []int64 {
	kept := ts[:0]
	for _, t := range ts {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	return kept
}

// aiTokensUsedSince sums totalTokens for an identity from _aiUsage since `t`.
func aiTokensUsedSince(app core.App, uid string, t time.Time) (int, error) {
	cutoff := t.UTC().Format("2006-01-02 15:04:05.000Z")
	var res struct {
		Total int `db:"total"`
	}
	err := app.DB().
		NewQuery("SELECT COALESCE(SUM(totalTokens),0) AS total FROM {{" + aiUsageCollection + "}} WHERE userId={:u} AND created>={:t}").
		Bind(dbx.Params{"u": uid, "t": cutoff}).
		One(&res)
	return res.Total, err
}

// enforceAILimits returns a 429 error if the caller is over a configured gate,
// or nil to proceed. Superusers (and API keys acting as superuser) are exempt.
func enforceAILimits(app core.App, e *core.RequestEvent, limits aiLimits) error {
	if e.Auth == nil || e.Auth.IsSuperuser() {
		return nil
	}
	uid := e.Auth.Id

	if limits.ratePerMin > 0 {
		if ok, n := aiRateWindow.allow(uid, limits.ratePerMin, time.Minute); !ok {
			return e.TooManyRequestsError(
				fmt.Sprintf("rate limit exceeded: max %d AI requests/min (%d in the last 60s)", limits.ratePerMin, n), nil)
		}
	}

	if limits.tokensPerDay > 0 {
		if used, err := aiTokensUsedSince(app, uid, time.Now().Add(-24*time.Hour)); err == nil && used >= limits.tokensPerDay {
			return e.TooManyRequestsError(
				fmt.Sprintf("daily token quota exhausted: %d/%d tokens in the last 24h", used, limits.tokensPerDay), nil)
		}
	}
	return nil
}
