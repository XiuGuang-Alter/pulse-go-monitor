package main

import (
	"context"
	"testing"
	"time"
)

// Catches false green status when SQLite accepts reads but rejects result writes.
func TestMonitorWriteFailureAndRecovery(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	target, err := store.SaveTarget(ctx, 0, inputFor("http://example.com"))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := NewScheduler(store, 1, func(context.Context, string, time.Duration) Result {
		return Result{Kind: "http_error", StatusCode: 500}
	})
	defer s.Stop()
	if _, err = store.db.Exec(`CREATE TRIGGER fail_write BEFORE INSERT ON checks BEGIN SELECT RAISE(FAIL, 'write denied'); END`); err != nil {
		t.Fatal(err)
	}
	s.scheduleDue()
	await(t, func() bool { return len(s.RunningIDs()) == 0 })
	h := s.Health(time.Now())
	if h.State != "degraded" || h.SuccessfulWrites != 0 || h.AffectedTargets != 1 {
		t.Fatalf("write failure hidden: %+v", h)
	}
	store.db.Exec(`DROP TRIGGER fail_write`)
	s.mu.Lock()
	delete(s.retryAt, target.ID)
	s.mu.Unlock()
	s.scheduleDue()
	await(t, func() bool { return len(s.RunningIDs()) == 0 })
	h = s.Health(time.Now())
	if h.State != "healthy" || h.SuccessfulWrites != 1 {
		t.Fatalf("HTTP500 is valid collection: %+v", h)
	}
}

func TestMonitorStartupIdleAndStalledScan(t *testing.T) {
	s, _ := NewScheduler(testStore(t), 1, nil)
	defer s.Stop()
	now := time.Now()
	if s.Health(now).State != "starting" {
		t.Fatal("not starting")
	}
	s.scheduleDue()
	h := s.Health(time.Now())
	if h.State != "idle" || h.InstanceID == "" {
		t.Fatalf("%+v", h)
	}
	if s.Health(time.Now().Add(time.Minute)).State != "degraded" {
		t.Fatal("stalled scan not detected")
	}
}

func TestMonitorReadFailureAndDeletion(t *testing.T) {
	store := testStore(t)
	s, _ := NewScheduler(store, 1, nil)
	defer s.Stop()
	target, _ := store.SaveTarget(context.Background(), 0, inputFor("http://example.com"))
	// Prevent launch while reconciling target lifetimes.
	store.db.Exec(`UPDATE targets SET next_check_at=?`, time.Now().Add(time.Hour).UnixMilli())
	s.mu.Lock()
	s.writeFailures[target.ID] = 2
	s.mu.Unlock()
	s.scheduleDue()
	if s.Health(time.Now()).State != "degraded" {
		t.Fatal("write failure lost")
	}
	store.DeleteTarget(context.Background(), target.ID)
	s.scheduleDue()
	if s.Health(time.Now()).State != "idle" {
		t.Fatal("deleted failure retained")
	}
	store.Close()
	s.scheduleDue()
	if s.Health(time.Now()).State != "degraded" {
		t.Fatal("read failure hidden")
	}
}

func TestMonitorFailuresAreIsolatedAndCountersReset(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	bad, _ := store.SaveTarget(ctx, 0, inputFor("http://example.com/bad"))
	good, _ := store.SaveTarget(ctx, 0, inputFor("http://example.com/good"))
	if _, err := store.db.Exec(`CREATE TRIGGER fail_one BEFORE INSERT ON checks WHEN NEW.target_id=1 BEGIN SELECT RAISE(FAIL, 'write denied'); END`); err != nil {
		t.Fatal(err)
	}
	s, _ := NewScheduler(store, 1, func(context.Context, string, time.Duration) Result { return Result{Kind: "ok"} })
	defer s.Stop()
	if err := s.Trigger(bad.ID); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return len(s.RunningIDs()) == 0 })
	if err := s.Trigger(good.ID); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return len(s.RunningIDs()) == 0 })
	s.scheduleDue()
	h := s.Health(time.Now())
	if h.State != "degraded" || h.AffectedTargets != 1 || h.SuccessfulWrites != 1 {
		t.Fatalf("other target cleared failure: %+v", h)
	}
	newProcess, _ := NewScheduler(store, 1, nil)
	defer newProcess.Stop()
	fresh := newProcess.Health(time.Now())
	if fresh.InstanceID == h.InstanceID || fresh.SuccessfulWrites != 0 || fresh.LastWriteAt != 0 {
		t.Fatalf("historical writes counted in new process: %+v", fresh)
	}
	input := bad.TargetInput
	input.Enabled = false
	if _, err := store.SaveTarget(ctx, bad.ID, input); err != nil {
		t.Fatal(err)
	}
	s.scheduleDue()
	if s.Health(time.Now()).State != "healthy" {
		t.Fatal("paused target kept monitor degraded")
	}
}

func TestMonitorReadFailureRecovers(t *testing.T) {
	store := testStore(t)
	s, _ := NewScheduler(store, 1, nil)
	defer s.Stop()
	if _, err := store.db.Exec(`ALTER TABLE targets RENAME TO unavailable_targets`); err != nil {
		t.Fatal(err)
	}
	s.scheduleDue()
	if h := s.Health(time.Now()); h.State != "degraded" || h.Reason != "storage_read_failed" {
		t.Fatalf("read failure hidden: %+v", h)
	}
	if _, err := store.db.Exec(`ALTER TABLE unavailable_targets RENAME TO targets`); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.readRetryAt = time.Time{}
	s.mu.Unlock()
	s.scheduleDue()
	if h := s.Health(time.Now()); h.State != "idle" || h.ConsecutiveErrors != 0 {
		t.Fatalf("recovery hidden: %+v", h)
	}
}
