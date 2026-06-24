// Command loadgen is a tiny, zero-dependency HTTP load generator for base-app.
// It runs a closed-loop (fixed-concurrency) load against one or more scenarios
// and reports throughput + latency percentiles.
//
// The three scenarios are chosen to isolate where base-app spends time:
//
//	read_public  GET records, NO auth        -> raw read path (SQLite read + JSON)
//	read_key     GET records, X-API-Key      -> read path + the API-key middleware
//	                                            (key lookup + the per-request
//	                                            lastUsedUnix WRITE). The gap vs
//	                                            read_public is the cost of that write.
//	write        POST a record, X-API-Key    -> the SQLite single-writer ceiling
//	                                            (+ Litestream if replication is on).
//
// Usage:
//
//	go run ./loadtest -base http://localhost:8090 -key pbk_... -scenario all -c 50 -d 20s
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	name    string
	reqs    int64
	errs    int64
	elapsed time.Duration
	lat     []time.Duration // successful-request latencies
}

func main() {
	base := flag.String("base", "http://localhost:8090", "base URL of the target instance")
	key := flag.String("key", "", "API key (pbk_...) for the read_key and write scenarios")
	scenario := flag.String("scenario", "all", "read_public | read_key | write | all")
	conc := flag.Int("c", 50, "concurrency (number of in-flight workers)")
	durStr := flag.String("d", "20s", "duration per scenario (e.g. 20s, 1m)")
	warmStr := flag.String("warmup", "3s", "warmup duration per scenario (not measured)")
	flag.Parse()

	dur, err := time.ParseDuration(*durStr)
	must(err, "bad -d")
	warm, err := time.ParseDuration(*warmStr)
	must(err, "bad -warmup")

	scenarios := []string{*scenario}
	if *scenario == "all" {
		scenarios = []string{"read_public", "read_key", "write"}
	}

	// Shared client with generous connection reuse so we measure the server, not
	// TCP/TLS handshakes.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *conc * 2,
			MaxIdleConnsPerHost: *conc * 2,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	fmt.Printf("target=%s  concurrency=%d  duration=%s/scenario  warmup=%s\n\n", *base, *conc, dur, warm)

	var results []result
	for _, s := range scenarios {
		if (s == "read_key" || s == "write") && *key == "" {
			fmt.Printf("skipping %-12s (no -key provided)\n", s)
			continue
		}
		reqFn, ok := requestBuilder(s, *base, *key)
		if !ok {
			fmt.Printf("unknown scenario %q\n", s)
			continue
		}
		fmt.Printf("running %-12s ...\n", s)
		runClosedLoop(client, reqFn, *conc, warm, true) // warmup (discarded)
		r := runClosedLoop(client, reqFn, *conc, dur, false)
		r.name = s
		results = append(results, r)
	}

	report(results)
}

// requestBuilder returns a closure that constructs a fresh *http.Request for the
// given scenario. The closure receives a per-request sequence number.
func requestBuilder(scenario, base, key string) (func(seq int64) *http.Request, bool) {
	recordsURL := base + "/api/collections/loadtest/records"
	switch scenario {
	case "read_public":
		return func(int64) *http.Request {
			req, _ := http.NewRequest(http.MethodGet, recordsURL+"?perPage=20", nil)
			return req
		}, true
	case "read_key":
		return func(int64) *http.Request {
			req, _ := http.NewRequest(http.MethodGet, recordsURL+"?perPage=20", nil)
			req.Header.Set("X-API-Key", key)
			return req
		}, true
	case "write":
		return func(seq int64) *http.Request {
			body := fmt.Sprintf(`{"title":"lt-%d","n":%d}`, seq, seq)
			req, _ := http.NewRequest(http.MethodPost, recordsURL, bytes.NewReader([]byte(body)))
			req.Header.Set("X-API-Key", key)
			req.Header.Set("Content-Type", "application/json")
			return req
		}, true
	}
	return nil, false
}

// runClosedLoop fires requests from `conc` workers for `dur`, each worker looping
// as fast as the server allows. Returns aggregate counts + per-request latencies.
func runClosedLoop(client *http.Client, build func(seq int64) *http.Request, conc int, dur time.Duration, warmup bool) result {
	deadline := time.Now().Add(dur)
	var reqs, errs int64
	var seq int64
	var mu sync.Mutex
	allLat := make([]time.Duration, 0, 1<<16)

	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, 1024)
			for time.Now().Before(deadline) {
				n := atomic.AddInt64(&seq, 1)
				req := build(n)
				t0 := time.Now()
				resp, err := client.Do(req)
				lat := time.Since(t0)
				atomic.AddInt64(&reqs, 1)
				if err != nil {
					atomic.AddInt64(&errs, 1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					local = append(local, lat)
				} else {
					atomic.AddInt64(&errs, 1)
				}
			}
			if !warmup {
				mu.Lock()
				allLat = append(allLat, local...)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return result{reqs: reqs, errs: errs, elapsed: time.Since(start), lat: allLat}
}

func report(results []result) {
	if len(results) == 0 {
		return
	}
	fmt.Printf("\n%-12s %8s %9s %8s %9s %9s %9s %9s\n",
		"scenario", "reqs", "rps", "err%", "p50", "p95", "p99", "max")
	fmt.Println(strings.Repeat("-", 80))
	byName := map[string]result{}
	for _, r := range results {
		byName[r.name] = r
		rps := float64(r.reqs) / r.elapsed.Seconds()
		errPct := 100 * float64(r.errs) / float64(max64(r.reqs, 1))
		fmt.Printf("%-12s %8d %9.0f %7.1f%% %9s %9s %9s %9s\n",
			r.name, r.reqs, rps, errPct,
			ms(pct(r.lat, 50)), ms(pct(r.lat, 95)), ms(pct(r.lat, 99)), ms(maxDur(r.lat)))
	}

	// The headline comparison: what does the per-request lastUsedUnix write cost?
	if rp, ok1 := byName["read_public"]; ok1 {
		if rk, ok2 := byName["read_key"]; ok2 && len(rp.lat) > 0 && len(rk.lat) > 0 {
			rpsP := float64(rp.reqs) / rp.elapsed.Seconds()
			rpsK := float64(rk.reqs) / rk.elapsed.Seconds()
			drop := 100 * (rpsP - rpsK) / rpsP
			fmt.Printf("\nAPI-key middleware tax (read_key vs read_public):\n")
			fmt.Printf("  throughput: %.0f -> %.0f rps  (%.0f%% lower)\n", rpsP, rpsK, drop)
			fmt.Printf("  p95 latency: %s -> %s\n", ms(pct(rp.lat, 95)), ms(pct(rk.lat, 95)))
			fmt.Printf("  ^ that gap is the per-request lastUsedUnix DB write in apikeys.go.\n")
		}
	}
}

func pct(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := make([]time.Duration, len(d))
	copy(s, d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := (p * len(s)) / 100
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

func maxDur(d []time.Duration) time.Duration {
	var m time.Duration
	for _, v := range d {
		if v > m {
			m = v
		}
	}
	return m
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000) }

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func must(err error, msg string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
		os.Exit(1)
	}
}
