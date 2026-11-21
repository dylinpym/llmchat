package main

// Spread-load driver: fires concurrent Send requests across MANY conversations,
// which is the realistic shape (each user has their own thread). hey pins one
// URL, so it can only model the worst case (everyone hammering one
// conversation). This driver varies the target per request and reports true
// percentiles from raw samples.
//
// It exists to measure the cost of the SELECT ... FOR UPDATE serialisation in
// AppendMessage under both shapes: contended (few conversations) vs spread (many).

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

func main() {
	base := "http://127.0.0.1:8080/api/v1/conversations/"
	body := []byte(`{"content":"bench turn"}`)

	raw, err := os.ReadFile("bench_ids.txt")
	if err != nil {
		fmt.Println("need bench_ids.txt:", err)
		os.Exit(1)
	}
	var ids []string
	for _, l := range strings.Split(string(raw), "\n") {
		if s := strings.TrimSpace(l); s != "" {
			ids = append(ids, s)
		}
	}
	if len(ids) == 0 {
		fmt.Println("no ids")
		os.Exit(1)
	}

	conc := 50
	total := 1000
	label := "spread-across-" + fmt.Sprint(len(ids)) + "-conversations"
	if v := os.Getenv("SPREAD_CONC"); v != "" {
		fmt.Sscanf(v, "%d", &conc)
	}
	if v := os.Getenv("SPREAD_TOTAL"); v != "" {
		fmt.Sscanf(v, "%d", &total)
	}
	if v := os.Getenv("SPREAD_LABEL"); v != "" {
		label = v
	}

	client := &http.Client{Timeout: 60 * time.Second}
	var (
		mu      sync.Mutex
		samples []float64
		errs    int
		ok      int
		next    int
		idxMu   sync.Mutex
	)

	start := time.Now()
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			idxMu.Lock()
			id := ids[next%len(ids)]
			next++
			idxMu.Unlock()

			t0 := time.Now()
			req, _ := http.NewRequest(http.MethodPost, base+id+"/messages", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-Id", "bench")
			resp, err := client.Do(req)
			dt := time.Since(t0).Seconds()
			if err != nil {
				mu.Lock()
				errs++
				mu.Unlock()
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			mu.Lock()
			samples = append(samples, dt)
			if resp.StatusCode == 200 {
				ok++
			} else {
				errs++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()

	sort.Float64s(samples)
	fmt.Printf("\n[%s] conc=%d total=%d\n", label, conc, total)
	fmt.Printf("  ok=%d err=%d  wall=%.2fs  throughput=%.1f req/s\n", ok, errs, elapsed, float64(ok)/elapsed)
	fmt.Printf("  latency ms: P50=%.1f P90=%.1f P99=%.1f max=%.1f\n",
		percentile(samples, 0.50)*1000, percentile(samples, 0.90)*1000,
		percentile(samples, 0.99)*1000, percentile(samples, 1.0)*1000)
}
