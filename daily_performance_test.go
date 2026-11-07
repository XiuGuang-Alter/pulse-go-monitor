package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkDaily30Days(b *testing.B) {
	s, err := OpenStore(filepath.Join(b.TempDir(), "daily.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 100; i++ {
		if _, err = s.SaveTarget(context.Background(), 0, TargetInput{Name: fmt.Sprint(i), URL: "http://127.0.0.1/synthetic", IntervalSeconds: 60, TimeoutSeconds: 1}); err != nil {
			b.Fatal(err)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	for id := 1; id <= 100; id++ {
		for day := 0; day < 30; day++ {
			_, err = tx.Exec("INSERT INTO daily_summaries VALUES(?,?,1000,900,50000,50,0)", id, utcDay(time.Now()).AddDate(0, 0, -day).Format("2006-01-02"))
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		result, err := s.Daily(context.Background(), 1, 30)
		if err != nil || len(result.Buckets) != 30 || result.Buckets[0].Samples != 1000 {
			b.Fatalf("unexpected summary: %+v %v", result, err)
		}
	}
}

func TestPerformanceQueryPlans(t *testing.T) {
	s := testStore(t)
	queries := []string{
		"SELECT c.id FROM targets t JOIN checks c ON c.id=(SELECT id FROM checks INDEXED BY checks_target_id WHERE target_id=t.id AND checked_at>=0 ORDER BY id DESC LIMIT 1)",
		"SELECT target_id,COUNT(*),SUM(CASE WHEN kind='ok' THEN 1 ELSE 0 END),AVG(duration_ms) FROM checks WHERE checked_at>=0 GROUP BY target_id",
		"SELECT * FROM checks INDEXED BY checks_target_id WHERE target_id=1 AND checked_at>=0 ORDER BY id DESC LIMIT 50",
		"SELECT * FROM daily_summaries WHERE target_id=1 AND date>='2000-01-01' AND date<='2100-01-01' ORDER BY date",
	}
	for _, query := range queries {
		t.Log(query)
		rows, err := s.db.Query("EXPLAIN QUERY PLAN " + query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
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
