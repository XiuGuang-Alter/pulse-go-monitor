package main

// This file intentionally compiles unchanged against v1.1 fbfd0be and v1.2.
// The runner copies it into separate source snapshots; their measurements are
// source benchmarks, not measurements of the user-supplied binary internals.
import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

type m4Categories interface {
	ErrorCategories(context.Context, int64) (map[string]int, error)
}

func m4Fixture(tb testing.TB) (*Store, Target) {
	tb.Helper()
	s, err := OpenStore(filepath.Join(tb.TempDir(), "synthetic-m4.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
	})
	ctx := context.Background()
	var first Target
	for id := 1; id <= 100; id++ {
		target, err := s.SaveTarget(ctx, 0, TargetInput{Name: fmt.Sprint("synthetic-", id), URL: "http://127.0.0.1/synthetic", IntervalSeconds: 86400, TimeoutSeconds: 5, Enabled: false})
		if err != nil {
			tb.Fatal(err)
		}
		if id == 1 {
			first = target
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	defer tx.Rollback()
	insert, err := tx.Prepare(`INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at,total_duration_ms,config_version,rules_snapshot) VALUES(?,?,?,?,?,?,?, ?,50,1,'{}')`)
	if err != nil {
		tb.Fatal(err)
	}
	defer insert.Close()
	kinds := []string{"ok", "http_error", "timeout", "assertion_failed", "request_error"}
	statuses := []int{200, 503, 0, 200, 0}
	today := utcDay(time.Now())
	for id := 1; id <= 100; id++ {
		counts, successes, sums, maxima := [30]int{}, [30]int{}, [30]float64{}, [30]float64{}
		for n := 0; n < 1000; n++ {
			day, kind := n%30, n%5
			duration := float64(50)
			if kind == 2 {
				duration = 1000
			}
			if _, err := insert.Exec(id, first.URL, first.URL, statuses[kind], duration, kinds[kind], "synthetic mixed fixture", today.AddDate(0, 0, -day).UnixMilli()); err != nil {
				tb.Fatal(err)
			}
			counts[day]++
			if kind == 0 {
				successes[day]++
			}
			sums[day] += duration
			if duration > maxima[day] {
				maxima[day] = duration
			}
		}
		for day := 0; day < 30; day++ {
			if _, err := tx.Exec(`INSERT INTO daily_summaries(target_id,date,samples,successes,duration_sum_ms,max_ms,migration_partial) VALUES(?,?,?,?,?,?,0)`, id, today.AddDate(0, 0, -day).Format("2006-01-02"), counts[day], successes[day], sums[day], maxima[day]); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	// The same legacy NULL rows exist in both versions. v1.2 infers categories
	// structurally (request_error -> unknown); no invented baseline category API.
	return s, first
}

func BenchmarkM4(b *testing.B) {
	s, target := m4Fixture(b)
	ctx := context.Background()
	b.Run("Store/List100x1000", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			rows, err := s.ListTargets(ctx)
			if err != nil || len(rows) != 100 {
				b.Fatalf("rows=%d err=%v", len(rows), err)
			}
		}
	})
	b.Run("Store/HistoryFirst50", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			rows, err := s.History(ctx, target.ID, 50, 0)
			if err != nil || len(rows) != 50 {
				b.Fatalf("rows=%d err=%v", len(rows), err)
			}
		}
	})
	b.Run("Store/HistoryNext50", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			rows, err := s.History(ctx, target.ID, 50, 951)
			if err != nil || len(rows) != 50 || rows[0].ID != 950 {
				b.Fatalf("rows=%d err=%v", len(rows), err)
			}
		}
	})
	b.Run("Store/Daily30", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			result, err := s.Daily(ctx, target.ID, 30)
			if err != nil || len(result.Buckets) != 30 {
				b.Fatalf("buckets=%d err=%v", len(result.Buckets), err)
			}
		}
	})
	b.Run("Store/ErrorCategories", func(b *testing.B) {
		categories, ok := any(s).(m4Categories)
		if !ok {
			b.Skip("N/A: v1.1 has no ErrorCategories method")
		}
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			result, err := categories.ErrorCategories(ctx, target.ID)
			if err != nil || len(result) != 4 {
				b.Fatalf("categories=%v err=%v", result, err)
			}
		}
	})
	scheduler, err := NewScheduler(s, 4, Check)
	if err != nil {
		b.Fatal(err)
	}
	defer scheduler.Stop()
	handler := NewApp(s, scheduler).Handler()
	for _, endpoint := range []struct{ name, route string }{
		{"List", "/api/targets"}, {"History50", "/api/targets/1/checks?limit=50"}, {"Daily30", "/api/targets/1/daily?days=30"}, {"Status", "/api/status"},
	} {
		b.Run("API/"+endpoint.name, func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				request := httptest.NewRequest("GET", "http://127.0.0.1"+endpoint.route, nil)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != 200 {
					b.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
				}
			}
		})
	}
	b.Run("Store/RecordCheckMixed", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			kinds := []string{"ok", "http_error", "timeout", "assertion_failed", "request_error"}
			statuses := []int{200, 503, 0, 200, 0}
			if err := s.RecordCheck(ctx, target, Result{URL: target.URL, Kind: kinds[n%5], StatusCode: statuses[n%5], Duration: 50 * time.Millisecond, Detail: "synthetic write"}, time.Now()); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestM4QueryPlans(t *testing.T) {
	s, _ := m4Fixture(t)
	queries := []struct{ name, sql string }{
		{"latest", `SELECT c.id FROM targets t JOIN checks c ON c.id=(SELECT id FROM checks INDEXED BY checks_target_id WHERE target_id=t.id AND checked_at>=0 ORDER BY id DESC LIMIT 1)`},
		{"list stats", `SELECT target_id,COUNT(*),SUM(CASE WHEN kind='ok' THEN 1 ELSE 0 END),AVG(duration_ms) FROM checks WHERE checked_at>=0 GROUP BY target_id`},
		{"history page", `SELECT * FROM checks INDEXED BY checks_target_id WHERE target_id=1 AND checked_at>=0 AND id<951 ORDER BY id DESC LIMIT 50`},
		{"daily", `SELECT * FROM daily_summaries WHERE target_id=1 AND date>='2000-01-01' AND date<='2100-01-01' ORDER BY date`},
	}
	if _, ok := any(s).(m4Categories); ok {
		queries = append(queries, struct{ name, sql string }{"categories", `SELECT kind,status_code,error_category,COUNT(*) FROM checks WHERE target_id=1 AND checked_at>=0 AND kind!='ok' GROUP BY kind,status_code,error_category`})
	} else {
		t.Log("categories: N/A in v1.1")
	}
	for _, query := range queries {
		t.Log(query.name + ": " + query.sql)
		rows, err := s.db.Query("EXPLAIN QUERY PLAN " + query.sql)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			t.Log(detail)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}
