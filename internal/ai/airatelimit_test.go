package ai

import (
	"sync"
	"testing"
	"time"
)

// pruneInPlace is the heart of the sliding window; test it with explicit cutoffs
// so there's no dependence on the wall clock.
func TestPruneInPlace(t *testing.T) {
	// cutoff = 100; keep strictly-greater timestamps.
	got := pruneInPlace([]int64{50, 99, 100, 101, 200}, 100)
	want := []int64{101, 200}
	if len(got) != len(want) {
		t.Fatalf("pruneInPlace len = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pruneInPlace = %v, want %v", got, want)
		}
	}
	// Everything expired.
	if g := pruneInPlace([]int64{1, 2, 3}, 100); len(g) != 0 {
		t.Errorf("expected empty, got %v", g)
	}
	// Empty input is safe.
	if g := pruneInPlace(nil, 100); len(g) != 0 {
		t.Errorf("expected empty for nil, got %v", g)
	}
}

// allow must admit up to `limit` hits per window, then reject — this is the
// request/min gate. Use a long window so nothing expires mid-test (deterministic).
func TestSlidingWindowAllowLimit(t *testing.T) {
	w := newSlidingWindow()
	const limit = 3
	for i := 1; i <= limit; i++ {
		ok, n := w.allow("user1", limit, time.Hour)
		if !ok {
			t.Fatalf("hit %d should be admitted", i)
		}
		if n != i {
			t.Fatalf("hit %d: count = %d, want %d", i, n, i)
		}
	}
	// Over the limit now.
	ok, n := w.allow("user1", limit, time.Hour)
	if ok {
		t.Fatalf("hit %d should be rejected", limit+1)
	}
	if n != limit {
		t.Errorf("rejected count = %d, want %d", n, limit)
	}
}

// Windows are per-identity: one user's traffic must not affect another's.
func TestSlidingWindowPerIdentity(t *testing.T) {
	w := newSlidingWindow()
	if ok, _ := w.allow("a", 1, time.Hour); !ok {
		t.Fatal("a first hit should be admitted")
	}
	if ok, _ := w.allow("a", 1, time.Hour); ok {
		t.Fatal("a second hit should be rejected")
	}
	// Different identity, still has its full budget.
	if ok, _ := w.allow("b", 1, time.Hour); !ok {
		t.Fatal("b first hit should be admitted independently of a")
	}
}

// Old hits must age out of the window so the budget recovers.
func TestSlidingWindowExpiry(t *testing.T) {
	w := newSlidingWindow()
	const window = 40 * time.Millisecond
	if ok, _ := w.allow("u", 1, window); !ok {
		t.Fatal("first hit should be admitted")
	}
	if ok, _ := w.allow("u", 1, window); ok {
		t.Fatal("immediate second hit should be rejected")
	}
	time.Sleep(window + 20*time.Millisecond)
	if ok, _ := w.allow("u", 1, window); !ok {
		t.Fatal("hit after the window elapsed should be admitted again")
	}
}

// count reports the window size WITHOUT recording a hit, and cleans up empty keys.
func TestSlidingWindowCount(t *testing.T) {
	w := newSlidingWindow()
	if c := w.count("ghost", time.Hour); c != 0 {
		t.Errorf("unknown identity count = %d, want 0", c)
	}
	w.allow("u", 10, time.Hour)
	w.allow("u", 10, time.Hour)
	if c := w.count("u", time.Hour); c != 2 {
		t.Errorf("count = %d, want 2", c)
	}
	// count must not itself consume budget.
	if c := w.count("u", time.Hour); c != 2 {
		t.Errorf("count is not read-only: second call = %d, want 2", c)
	}
}

// Concurrent callers must never exceed the limit and the map must stay race-free.
// Run with -race to actually exercise the mutex. With limit N and many goroutines
// each firing one hit, exactly N must be admitted.
func TestSlidingWindowConcurrent(t *testing.T) {
	w := newSlidingWindow()
	const limit = 50
	const goroutines = 500

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if ok, _ := w.allow("shared", limit, time.Hour); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if admitted != limit {
		t.Errorf("admitted %d concurrent hits, want exactly %d", admitted, limit)
	}
	if c := w.count("shared", time.Hour); c != limit {
		t.Errorf("window count = %d, want %d", c, limit)
	}
}

func TestEnvInt(t *testing.T) {
	t.Setenv("AI_TEST_INT", "42")
	if got := envInt("AI_TEST_INT", 7); got != 42 {
		t.Errorf("envInt set = %d, want 42", got)
	}
	t.Setenv("AI_TEST_INT", "not-a-number")
	if got := envInt("AI_TEST_INT", 7); got != 7 {
		t.Errorf("envInt invalid = %d, want default 7", got)
	}
	if got := envInt("AI_TEST_UNSET_VAR", 99); got != 99 {
		t.Errorf("envInt unset = %d, want default 99", got)
	}
}

func TestAILimitsFromEnv(t *testing.T) {
	// Defaults when unset.
	t.Setenv("AI_RATE_LIMIT_PER_MIN", "")
	t.Setenv("AI_TOKEN_QUOTA_PER_DAY", "")
	if l := aiLimitsFromEnv(); l.ratePerMin != 60 || l.tokensPerDay != 0 {
		t.Errorf("defaults = %+v, want {ratePerMin:60 tokensPerDay:0}", l)
	}
	// Overrides.
	t.Setenv("AI_RATE_LIMIT_PER_MIN", "10")
	t.Setenv("AI_TOKEN_QUOTA_PER_DAY", "100000")
	if l := aiLimitsFromEnv(); l.ratePerMin != 10 || l.tokensPerDay != 100000 {
		t.Errorf("overrides = %+v, want {ratePerMin:10 tokensPerDay:100000}", l)
	}
}
