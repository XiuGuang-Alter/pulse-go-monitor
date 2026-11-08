package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFreshnessBoundaries(t *testing.T) {
	now := time.UnixMilli(100000)
	version := int64(2)
	base := Target{TargetInput: TargetInput{Enabled: true, TimeoutSeconds: 5}, Version: 2, NextCheckAt: 100000, Latest: &CheckRecord{Kind: "ok", ConfigVersion: &version}}
	for _, tc := range []struct {
		name string
		edit func(*Target)
		at   time.Time
		want string
	}{
		{"fresh", func(*Target) {}, now, "fresh"},
		{"grace boundary", func(*Target) {}, now.Add(7 * time.Second), "fresh"},
		{"stale", func(*Target) {}, now.Add(7*time.Second + time.Millisecond), "stale"},
		{"paused", func(t *Target) { t.Enabled = false }, now.Add(time.Hour), "paused"},
		{"running", func(t *Target) { t.Running = true }, now, "checking"},
		{"queued", func(t *Target) { t.Latest = nil }, now, "waiting"},
		{"never sampled", func(t *Target) { t.Latest = nil; t.NextCheckAt = 200000 }, now, "unsampled"},
		{"new config", func(t *Target) { t.Version = 3 }, now, "pending_configuration"},
		{"old sample", func(t *Target) { t.Latest = &CheckRecord{Kind: "ok"} }, now, "pending_configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := base
			tc.edit(&target)
			if got := targetFreshness(target, tc.at); got.State != tc.want {
				t.Fatalf("got %+v want %s", got, tc.want)
			}
		})
	}
}

func TestStatusExposesProcessHealth(t *testing.T) {
	s, _ := NewScheduler(testStore(t), 1, nil)
	defer s.Stop()
	s.scheduleDue()
	rr := httptest.NewRecorder()
	NewApp(s.store, s).Handler().ServeHTTP(rr, httptest.NewRequest("GET", "http://127.0.0.1:8080/api/status", nil))
	var body struct {
		Status string        `json:"status"`
		Health MonitorHealth `json:"monitor_health"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || body.Status != "ok" || body.Health.State != "idle" {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestFreshnessUsesInFlightTimeoutSnapshot(t *testing.T) {
	target := Target{TargetInput: TargetInput{Enabled: true, TimeoutSeconds: 1}, Running: true, RunningSince: 100000, RunningDeadline: 132000, NextCheckAt: 100000}
	if got := targetFreshness(target, time.UnixMilli(104000)); got.State != "checking" {
		t.Fatalf("old 30s request falsely stale after editing timeout: %+v", got)
	}
	if got := targetFreshness(target, time.UnixMilli(132001)); got.State != "stale" {
		t.Fatalf("hung old request not stale: %+v", got)
	}
}

func TestFreshnessExplainsQueueWithExistingSample(t *testing.T) {
	version := int64(1)
	target := Target{TargetInput: TargetInput{Enabled: true, TimeoutSeconds: 30}, Version: 1, NextCheckAt: 100000, Latest: &CheckRecord{Kind: "ok", ConfigVersion: &version}}
	f := targetFreshness(target, time.UnixMilli(110000))
	if f.State != "fresh" || f.Reason != "queued" {
		t.Fatalf("queue hidden: %+v", f)
	}
	f = targetFreshness(target, time.UnixMilli(132001))
	if f.State != "stale" || f.Reason != "schedule_delayed" {
		t.Fatalf("overdue queue hidden: %+v", f)
	}
}
