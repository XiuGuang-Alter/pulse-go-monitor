package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHistoryExplainsActualAndTheoreticalRetention(t *testing.T) {
	store := testStore(t)
	in := inputFor("http://example.com")
	in.IntervalSeconds = 60
	target, err := store.SaveTarget(context.Background(), 0, in)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	last := first.Add(time.Minute)
	for _, at := range []time.Time{first, last} {
		if err = store.RecordCheck(context.Background(), target, Result{Kind: "ok"}, at); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := NewScheduler(store, 1, nil)
	defer s.Stop()
	rr := httptest.NewRecorder()
	NewApp(store, s).Handler().ServeHTTP(rr, httptest.NewRequest("GET", "http://127.0.0.1/api/targets/1/checks", nil))
	var body struct {
		Retention struct {
			MaxRecords         int    `json:"max_records"`
			MaxAgeDays         int    `json:"max_age_days"`
			TheoreticalSeconds int64  `json:"theoretical_seconds"`
			Records            int    `json:"records"`
			Earliest           *int64 `json:"earliest_checked_at"`
			Latest             *int64 `json:"latest_checked_at"`
		} `json:"retention"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	r := body.Retention
	if r.MaxRecords != 1000 || r.MaxAgeDays != 30 || r.TheoreticalSeconds != 60000 || r.Records != 2 || r.Earliest == nil || *r.Earliest != first.UnixMilli() || r.Latest == nil || *r.Latest != last.UnixMilli() {
		t.Fatalf("missing/misleading retention: %s", rr.Body.String())
	}
}

func TestRetentionEmptyAndAgeLimit(t *testing.T) {
	store := testStore(t)
	in := inputFor("http://example.com")
	in.IntervalSeconds = 86400
	target, err := store.SaveTarget(context.Background(), 0, in)
	if err != nil {
		t.Fatal(err)
	}
	r, err := store.Retention(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.TheoreticalSeconds != 2592000 || r.Records != 0 || r.EarliestCheckedAt != nil || r.LatestCheckedAt != nil {
		t.Fatalf("%+v", r)
	}
}
