// Command kerberon-bench measures ingest throughput and end-to-end latency
// against a running Kerberon.
//
// It exists so the numbers in the README are measured rather than asserted.
// Publishing a throughput figure nobody has reproduced is how a project loses
// the credibility it was trying to buy (spec section 11).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kerberon-bench:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		target      = flag.String("url", "http://127.0.0.1:8099", "Kerberon base URL")
		token       = flag.String("token", "", "ingest token")
		total       = flag.Int("alerts", 20000, "total alerts to send")
		batch       = flag.Int("batch", 200, "alerts per request, as Alertmanager would batch them")
		concurrency = flag.Int("concurrency", 8, "concurrent senders")
		groups      = flag.Int("groups", 20, "distinct incident groups to spread alerts across")
		timeout     = flag.Duration("timeout", 60*time.Second, "per-request timeout")
	)
	flag.Parse()

	if *token == "" {
		return fmt.Errorf("--token is required")
	}
	if *batch < 1 {
		*batch = 1
	}

	client := &http.Client{Timeout: *timeout}
	url := *target + "/api/v1/alerts"

	requests := (*total + *batch - 1) / *batch
	fmt.Printf("sending %d alerts in %d requests of %d, across %d groups, %d senders\n",
		*total, requests, *batch, *groups, *concurrency)

	var (
		sent      atomic.Int64
		accepted  atomic.Int64
		deduped   atomic.Int64
		incidents atomic.Int64
		failed    atomic.Int64

		mu        sync.Mutex
		latencies []time.Duration
	)

	work := make(chan int, requests)
	for i := 0; i < requests; i++ {
		work <- i
	}
	close(work)

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))

			for i := range work {
				body := payload(rng, i, *batch, *groups)
				reqStart := time.Now()

				req, err := http.NewRequestWithContext(context.Background(),
					http.MethodPost, url, bytes.NewReader(body))
				if err != nil {
					failed.Add(1)
					continue
				}
				req.Header.Set("Authorization", "Bearer "+*token)
				req.Header.Set("Content-Type", "application/json")

				resp, err := client.Do(req)
				if err != nil {
					failed.Add(1)
					continue
				}
				var out struct {
					Accepted     int `json:"accepted"`
					IncidentsNew int `json:"incidents_created"`
					Deduplicated int `json:"deduplicated"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&out)
				resp.Body.Close()

				elapsed := time.Since(reqStart)
				if resp.StatusCode != http.StatusOK {
					failed.Add(1)
					continue
				}

				sent.Add(int64(*batch))
				accepted.Add(int64(out.Accepted))
				deduped.Add(int64(out.Deduplicated))
				incidents.Add(int64(out.IncidentsNew))

				mu.Lock()
				latencies = append(latencies, elapsed)
				mu.Unlock()
			}
		}(int64(w) + 1)
	}
	wg.Wait()
	elapsed := time.Since(start)

	mu.Lock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	mu.Unlock()

	fmt.Println()
	fmt.Println("─── results ───────────────────────────────────────────")
	fmt.Printf("  wall time            %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  alerts accepted      %d\n", accepted.Load())
	fmt.Printf("  incidents created    %d\n", incidents.Load())
	fmt.Printf("  deduplicated         %d\n", deduped.Load())
	fmt.Printf("  failed requests      %d\n", failed.Load())
	if elapsed > 0 {
		fmt.Printf("  ingest throughput    %.0f alerts/sec\n",
			float64(accepted.Load())/elapsed.Seconds())
		fmt.Printf("  request throughput   %.0f req/sec\n",
			float64(len(latencies))/elapsed.Seconds())
	}
	if n := len(latencies); n > 0 {
		fmt.Printf("  request latency      p50 %s   p95 %s   p99 %s   max %s\n",
			latencies[n*50/100].Round(time.Microsecond*100),
			latencies[min(n*95/100, n-1)].Round(time.Microsecond*100),
			latencies[min(n*99/100, n-1)].Round(time.Microsecond*100),
			latencies[n-1].Round(time.Microsecond*100))
	}
	if accepted.Load() > 0 {
		fmt.Printf("  dedup ratio          %.1f%% of alerts collapsed\n",
			100*float64(deduped.Load())/float64(accepted.Load()))
	}
	if failed.Load() > 0 {
		return fmt.Errorf("%d requests failed", failed.Load())
	}
	return nil
}

// payload builds one batched request.
//
// Alerts are spread across a fixed number of groups and vary only in a
// volatile label, which is what a real cascade looks like: many alerts, few
// underlying problems.
func payload(rng *rand.Rand, seq, batch, groups int) []byte {
	type alert struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	}
	out := struct {
		Alerts []alert `json:"alerts"`
	}{Alerts: make([]alert, 0, batch)}

	for i := 0; i < batch; i++ {
		g := (seq*batch + i) % groups
		out.Alerts = append(out.Alerts, alert{
			Status: "firing",
			Labels: map[string]string{
				"alertname": fmt.Sprintf("BenchAlert%d", g),
				"cluster":   "prod",
				"severity":  "critical",
				// Volatile, so these collapse into one incident per group,
				// exercising the dedup path rather than avoiding it.
				"pod": fmt.Sprintf("pod-%d", rng.Int63()),
			},
			Annotations: map[string]string{"summary": fmt.Sprintf("bench group %d", g)},
		})
	}
	b, _ := json.Marshal(out)
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
