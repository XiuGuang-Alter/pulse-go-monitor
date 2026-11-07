package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func inputFor(url string) TargetInput {
	return TargetInput{Name: "测试接口", URL: url, IntervalSeconds: 5, TimeoutSeconds: 1, Enabled: true}
}

func TestStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "monitor.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.SaveTarget(ctx, 0, inputFor("https://example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if target.ID <= 0 || target.NextCheckAt > time.Now().UnixMilli() {
		t.Fatalf("bad target %+v", target)
	}
	for _, kind := range []string{"ok", "http_error", "ok"} {
		if err = s.RecordCheck(ctx, target, Result{URL: target.URL, Kind: kind, StatusCode: 200, Duration: 10 * time.Millisecond, Detail: kind}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := s.ListTargets(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if list[0].Latest == nil || list[0].Stats.Samples != 3 || list[0].Stats.Successes != 2 {
		t.Fatalf("stats %+v", list[0])
	}
	page, err := s.History(ctx, target.ID, 2, 0)
	if err != nil || len(page) != 2 {
		t.Fatalf("history %v %v", page, err)
	}
	older, err := s.History(ctx, target.ID, 2, page[1].ID)
	if err != nil || len(older) != 1 || older[0].ID >= page[1].ID {
		t.Fatal("broken pagination", older, err)
	}
	changed := target.TargetInput
	changed.Name = "新名称"
	changed.Enabled = false
	updated, err := s.SaveTarget(ctx, target.ID, changed)
	if err != nil || updated.Version != target.Version+1 {
		t.Fatal(updated, err)
	}
	next := updated.NextCheckAt
	if err = s.RecordCheck(ctx, target, Result{URL: target.URL, Kind: "ok"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	updated, _ = s.GetTarget(ctx, target.ID)
	if updated.NextCheckAt != next || updated.Enabled {
		t.Fatal("stale check overwrote configuration")
	}
	if err = s.DeleteTarget(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetTarget(ctx, target.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err = s.RecordCheck(ctx, target, Result{Kind: "ok"}, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted target revived", err)
	}
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM checks").Scan(&count)
	if count != 0 {
		t.Fatal("cascade failed")
	}
}

func TestStoreValidationAndRetention(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, edit := range []func(*TargetInput){func(i *TargetInput) { i.Name = "" }, func(i *TargetInput) { i.URL = "example.com" }, func(i *TargetInput) { i.IntervalSeconds = 4 }, func(i *TargetInput) { i.TimeoutSeconds = 31 }, func(i *TargetInput) { i.URL = "http://user:pass@example.com" }} {
		in := inputFor("https://example.com")
		edit(&in)
		if _, err := s.SaveTarget(ctx, 0, in); !errors.Is(err, ErrValidation) {
			t.Fatalf("accepted invalid input %+v: %v", in, err)
		}
	}
	target, _ := s.SaveTarget(ctx, 0, inputFor("https://example.com"))
	// 直接准备旧历史，验证真实记录路径会清理过期和超过上限的数据。
	tx, _ := s.db.Begin()
	for n := 0; n < 1002; n++ {
		_, err := tx.Exec("INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at) VALUES(?,?,?,?,?,?,?,?)", target.ID, target.URL, target.URL, 200, 1, "ok", "old", time.Now().Add(-31*24*time.Hour).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
	}
	tx.Commit()
	if err := s.RecordCheck(ctx, target, Result{Kind: "ok", URL: target.URL}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.History(ctx, target.ID, 200, 0)
	if len(rows) != 1 {
		t.Fatalf("retention got %d rows", len(rows))
	}
	if _, err := s.History(ctx, target.ID, 0, 0); !errors.Is(err, ErrValidation) {
		t.Fatal("zero page size accepted")
	}
	if err := s.DeleteTarget(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestStoreHidesExpiredHistoryAndLimitsSamples(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target, err := s.SaveTarget(ctx, 0, inputFor("https://example.com"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 1002; n++ {
		_, err = tx.Exec("INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at) VALUES(?,?,?,?,?,?,?,?)", target.ID, target.URL, target.URL, 200, 1, "ok", "fixture", time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordCheck(ctx, target, Result{Kind: "ok", URL: target.URL}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM checks WHERE target_id=?", target.ID).Scan(&count)
	if count != 1000 {
		t.Fatalf("retained %d, want 1000", count)
	}
	if _, err = s.db.Exec("UPDATE checks SET checked_at=?", time.Now().Add(-31*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.History(ctx, target.ID, 10, 0)
	if err != nil || len(rows) != 0 {
		t.Fatal("expired paused history still visible", len(rows), err)
	}
	list, err := s.ListTargets(ctx)
	if err != nil || list[0].Stats.Samples != 0 || list[0].Latest != nil {
		t.Fatal("expired samples included in metrics", list, err)
	}
}
