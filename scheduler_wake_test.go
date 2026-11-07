package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerCompletionWake(t *testing.T) {
	store := testStore(t)
	for i := 0; i < 3; i++ {
		in := inputFor("http://example.com")
		in.IntervalSeconds = 86400
		if _, err := store.SaveTarget(context.Background(), 0, in); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan struct{}, 3)
	release := make(chan struct{}, 3)
	s, _ := NewScheduler(store, 1, func(ctx context.Context, _ string, _ time.Duration) Result {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Result{Kind: "ok"}
	})
	s.pollInterval = 0 // Completion must wake the dispatcher without a periodic tick.
	s.Start()
	defer s.Stop()
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("completion did not refill slot")
		}
		release <- struct{}{}
	}
}

func TestSchedulerConfigurationWake(t *testing.T) {
	store := testStore(t)
	in := inputFor("http://example.com")
	in.Enabled = false
	target, _ := store.SaveTarget(context.Background(), 0, in)
	started := make(chan struct{}, 1)
	s, _ := NewScheduler(store, 1, func(context.Context, string, time.Duration) Result { started <- struct{}{}; return Result{Kind: "ok"} })
	s.pollInterval = 0
	s.Start()
	defer s.Stop()
	<-s.ready
	in.Enabled = true
	store.SaveTarget(context.Background(), target.ID, in)
	for i := 0; i < 100; i++ {
		s.Notify()
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("configuration wake lost")
	}
}

func TestSchedulerStorageFailureBackoff(t *testing.T) {
	store := testStore(t)
	store.SaveTarget(context.Background(), 0, inputFor("http://example.com"))
	if _, err := store.db.Exec(`CREATE TRIGGER reject_check BEFORE INSERT ON checks BEGIN SELECT RAISE(ABORT,'test write failure'); END`); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s, _ := NewScheduler(store, 1, func(context.Context, string, time.Duration) Result { calls.Add(1); return Result{Kind: "ok"} })
	s.pollInterval = 10 * time.Millisecond
	s.Start()
	defer s.Stop()
	await(t, func() bool { return calls.Load() == 1 })
	time.Sleep(150 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("failed writes retried without backoff: %d", got)
	}
	if _, err := store.db.Exec("DROP TRIGGER reject_check"); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return calls.Load() >= 2 })
}
