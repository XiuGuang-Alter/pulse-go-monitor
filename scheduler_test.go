package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func await(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestSchedulerBoundsAndStop(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	var targets []Target
	for n := 0; n < 3; n++ {
		in := inputFor("https://example.com")
		in.Enabled = false
		target, _ := store.SaveTarget(ctx, 0, in)
		targets = append(targets, target)
	}
	var started atomic.Int32
	scheduler, err := NewScheduler(store, 2, func(ctx context.Context, url string, timeout time.Duration) Result {
		started.Add(1)
		<-ctx.Done()
		return Result{Kind: "request_error"}
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler.Start()
	defer scheduler.Stop()
	if err = scheduler.Trigger(targets[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = scheduler.Trigger(targets[0].ID); !errors.Is(err, ErrBusy) {
		t.Fatal("duplicate allowed", err)
	}
	if err = scheduler.Trigger(targets[1].ID); err != nil {
		t.Fatal(err)
	}
	if err = scheduler.Trigger(targets[2].ID); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrency limit ignored", err)
	}
	await(t, func() bool { return started.Load() == 2 })
	done := make(chan struct{})
	go func() { scheduler.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop hung")
	}
	if err = scheduler.Trigger(targets[0].ID); !errors.Is(err, ErrStopped) {
		t.Fatal(err)
	}
	history, _ := store.History(ctx, targets[0].ID, 10, 0)
	if len(history) != 0 {
		t.Fatal("shutdown cancellation recorded as failure")
	}
}

func TestSchedulerAutomaticAndPaused(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	target, _ := store.SaveTarget(ctx, 0, inputFor("https://example.com"))
	pausedInput := inputFor("https://example.com/paused")
	pausedInput.Enabled = false
	paused, _ := store.SaveTarget(ctx, 0, pausedInput)
	scheduler, _ := NewScheduler(store, 1, func(ctx context.Context, url string, timeout time.Duration) Result {
		return Result{URL: url, Kind: "ok", StatusCode: 200, Detail: "test"}
	})
	scheduler.Start()
	defer scheduler.Stop()
	count := func(id int64) int { records, _ := store.History(ctx, id, 10, 0); return len(records) }
	await(t, func() bool { return count(target.ID) == 1 })
	if count(paused.ID) != 0 {
		t.Fatal("paused target auto checked")
	}
	store.db.Exec("UPDATE targets SET next_check_at=0 WHERE id=?", target.ID)
	await(t, func() bool { return count(target.ID) == 2 })
	await(t, func() bool { return len(scheduler.RunningIDs()) == 0 })
	if err := scheduler.Trigger(paused.ID); err != nil {
		t.Fatal("manual check of paused target denied", err)
	}
	await(t, func() bool { return count(paused.ID) == 1 })
}

// A channel holds the actual task between taking its configuration snapshot
// and persisting its result. No sleeps decide when an edit/delete takes place.
func TestSchedulerConfigurationChangesDuringCheck(t *testing.T) {
	for _, action := range []string{"edit", "pause", "delete"} {
		t.Run(action, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			target, err := store.SaveTarget(ctx, 0, inputFor("https://example.com/old"))
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan string, 2)
			release := make(chan struct{})
			scheduler, err := NewScheduler(store, 1, func(ctx context.Context, url string, timeout time.Duration) Result {
				started <- url
				select {
				case <-release:
				case <-ctx.Done():
				}
				return Result{URL: url, Kind: "ok", StatusCode: 200, Detail: "controlled completion"}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer scheduler.Stop()
			if err := scheduler.Trigger(target.ID); err != nil {
				t.Fatal(err)
			}
			select {
			case url := <-started:
				if url != "https://example.com/old" {
					t.Fatalf("wrong first URL: %s", url)
				}
			case <-time.After(time.Second):
				t.Fatal("check did not start")
			}
			var updated Target
			if action == "delete" {
				err = store.DeleteTarget(ctx, target.ID)
			} else {
				in := target.TargetInput
				if action == "edit" {
					in.URL = "https://example.com/new"
					in.IntervalSeconds = 120
					in.TimeoutSeconds = 7
				} else {
					in.Enabled = false
				}
				updated, err = store.SaveTarget(ctx, target.ID, in)
			}
			if err != nil {
				t.Fatal(err)
			}
			close(release)
			await(t, func() bool { return len(scheduler.RunningIDs()) == 0 })
			if action == "delete" {
				if _, err := store.GetTarget(ctx, target.ID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("deleted target returned: %v", err)
				}
				var count int
				if err := store.db.QueryRow("SELECT COUNT(*) FROM checks").Scan(&count); err != nil || count != 0 {
					t.Fatalf("deleted target gained history: %d %v", count, err)
				}
				scheduler.scheduleDue()
				if err := scheduler.Trigger(target.ID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("deleted target can be triggered: %v", err)
				}
				return
			}
			current, err := store.GetTarget(ctx, target.ID)
			if err != nil || current.TargetInput != updated.TargetInput || current.NextCheckAt != updated.NextCheckAt {
				t.Fatalf("old task changed edited configuration/schedule: %+v want %+v err=%v", current, updated, err)
			}
			history, err := store.History(ctx, target.ID, 10, 0)
			if err != nil || len(history) != 1 {
				t.Fatalf("in-flight result missing: %+v %v", history, err)
			}
			if history[0].InputURL != "https://example.com/old" || history[0].Kind != "ok" {
				t.Fatalf("result lost its input snapshot: %+v", history[0])
			}
			scheduler.scheduleDue()
			if action == "pause" {
				if len(scheduler.RunningIDs()) != 0 {
					t.Fatal("paused target launched another automatic check")
				}
				select {
				case url := <-started:
					t.Fatalf("paused target checked: %s", url)
				default:
				}
			} else {
				select {
				case url := <-started:
					if url != "https://example.com/new" {
						t.Fatalf("next check ignored new URL: %s", url)
					}
				case <-time.After(time.Second):
					t.Fatal("updated target was not checked when due")
				}
				await(t, func() bool { return len(scheduler.RunningIDs()) == 0 })
				history, err := store.History(ctx, target.ID, 10, 0)
				if err != nil || len(history) != 2 || history[0].InputURL != "https://example.com/new" {
					t.Fatalf("new configuration result not saved: %+v %v", history, err)
				}
			}
		})
	}
}
