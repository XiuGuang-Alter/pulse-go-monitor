package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in experiment: real HTTP, scheduler and SQLite; never performance claims
// derived from a fake Checker or from user monitoring data.
func TestPerformanceMatrix(t *testing.T) {
	output := os.Getenv("PULSE_PERFORMANCE_OUT")
	if output == "" {
		t.Skip("set PULSE_PERFORMANCE_OUT to run the measured experiment")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := csv.NewWriter(file)
	defer writer.Flush()
	writer.Write([]string{"go", "os", "arch", "cpus", "targets", "concurrency", "round", "upstream_delay_ms", "elapsed_ms", "completed_per_second", "peak_active", "peak_goroutines", "peak_heap_bytes", "heap_growth_bytes"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	for _, count := range []int{10, 100} {
		for _, concurrency := range []int{1, 4, 16} {
			for round := 1; round <= 3; round++ {
				t.Run(fmt.Sprintf("targets%d/concurrency%d/round%d", count, concurrency, round), func(t *testing.T) {
					store := testStore(t)
					for index := 0; index < count; index++ {
						input := TargetInput{Name: fmt.Sprintf("synthetic-%d", index), URL: fmt.Sprintf("%s/check/%d", server.URL, index), IntervalSeconds: 86400, TimeoutSeconds: 5, Enabled: true}
						if _, err := store.SaveTarget(context.Background(), 0, input); err != nil {
							t.Fatal(err)
						}
					}
					var active, peak atomic.Int32
					var mu sync.Mutex
					running := map[string]bool{}
					violation := make(chan string, 1)
					checker := func(ctx context.Context, url string, timeout time.Duration) Result {
						n := active.Add(1)
						for {
							old := peak.Load()
							if n <= old || peak.CompareAndSwap(old, n) {
								break
							}
						}
						mu.Lock()
						if running[url] {
							select {
							case violation <- "overlapping target":
							default:
							}
						}
						running[url] = true
						mu.Unlock()
						defer func() { mu.Lock(); delete(running, url); mu.Unlock(); active.Add(-1) }()
						return Check(ctx, url, timeout)
					}
					scheduler, err := NewScheduler(store, concurrency, checker)
					if err != nil {
						t.Fatal(err)
					}
					defer scheduler.Stop()
					runtime.GC()
					var memory runtime.MemStats
					runtime.ReadMemStats(&memory)
					baseline := memory.HeapAlloc
					peakHeap := baseline
					peakGo := runtime.NumGoroutine()
					started := time.Now()
					scheduler.Start()
					deadline := started.Add(90 * time.Second)
					for {
						var finished int
						if err := store.db.QueryRow("SELECT COUNT(*) FROM checks").Scan(&finished); err != nil {
							t.Fatal(err)
						}
						runtime.ReadMemStats(&memory)
						if memory.HeapAlloc > peakHeap {
							peakHeap = memory.HeapAlloc
						}
						if n := runtime.NumGoroutine(); n > peakGo {
							peakGo = n
						}
						if finished == count {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("only %d/%d completed", finished, count)
						}
						time.Sleep(10 * time.Millisecond)
					}
					elapsed := time.Since(started)
					scheduler.Stop()
					var completed, distinct, failures int
					if err := store.db.QueryRow("SELECT COUNT(*),COUNT(DISTINCT target_id),COALESCE(SUM(CASE WHEN kind!='ok' THEN 1 ELSE 0 END),0) FROM checks").Scan(&completed, &distinct, &failures); err != nil {
						t.Fatal(err)
					}
					if completed != count || distinct != count || failures != 0 || int(peak.Load()) > concurrency || active.Load() != 0 {
						t.Fatalf("count=%d distinct=%d failures=%d peak=%d active=%d", completed, distinct, failures, peak.Load(), active.Load())
					}
					select {
					case issue := <-violation:
						t.Fatal(issue)
					default:
					}
					row := []string{runtime.Version(), runtime.GOOS, runtime.GOARCH, strconv.Itoa(runtime.NumCPU()), strconv.Itoa(count), strconv.Itoa(concurrency), strconv.Itoa(round), "50", fmt.Sprintf("%.3f", float64(elapsed)/float64(time.Millisecond)), fmt.Sprintf("%.3f", float64(count)/elapsed.Seconds()), fmt.Sprint(peak.Load()), fmt.Sprint(peakGo), fmt.Sprint(peakHeap), fmt.Sprint(peakHeap - baseline)}
					if err := writer.Write(row); err != nil {
						t.Fatal(err)
					}
					writer.Flush()
					if err := writer.Error(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func BenchmarkStoreQueries(b *testing.B) {
	store, err := OpenStore(filepath.Join(b.TempDir(), "synthetic.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	var firstID int64
	for n := 0; n < 100; n++ {
		target, err := store.SaveTarget(context.Background(), 0, TargetInput{Name: fmt.Sprintf("synthetic-%d", n), URL: "http://127.0.0.1/synthetic", IntervalSeconds: 60, TimeoutSeconds: 1, Enabled: false})
		if err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			firstID = target.ID
		}
	}
	tx, err := store.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare("INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at) VALUES(?,?,?,200,50,'ok','synthetic benchmark fixture',?)")
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for target := int64(1); target <= 100; target++ {
		for n := 0; n < 1000; n++ {
			if _, err := statement.Exec(target, "http://127.0.0.1/synthetic", "http://127.0.0.1/synthetic", now); err != nil {
				b.Fatal(err)
			}
		}
	}
	statement.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.Run("List100Targets1000SamplesEach", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			rows, err := store.ListTargets(context.Background())
			if err != nil || len(rows) != 100 {
				b.Fatalf("rows=%d err=%v", len(rows), err)
			}
		}
	})
	b.Run("History50Of1000", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			rows, err := store.History(context.Background(), firstID, 50, 0)
			if err != nil || len(rows) != 50 {
				b.Fatalf("rows=%d err=%v", len(rows), err)
			}
		}
	})
}
