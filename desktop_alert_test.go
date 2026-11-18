package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type desktopNotifierStub struct {
	sendEvent func(context.Context, desktopAlert) (bool, error)
	closed    bool
}

func (n *desktopNotifierStub) send(ctx context.Context, event desktopAlert) (bool, error) {
	return n.sendEvent(ctx, event)
}
func (*desktopNotifierStub) pump()    {}
func (n *desktopNotifierStub) close() { n.closed = true }

func TestDesktopAlertDeliveryKeepsMonitorIndependent(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := TargetInput{Name: "local target", URL: "http://example.invalid/", Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5}
	target, err := store.SaveTarget(ctx, 0, input)
	if err != nil {
		t.Fatal(err)
	}
	checker := func(context.Context, string, time.Duration) Result { return Result{Kind: "timeout"} }
	scheduler, err := NewScheduler(store, 1, checker)
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	started, release := make(chan struct{}), make(chan struct{})
	notifier := &desktopNotifierStub{sendEvent: func(ctx context.Context, _ desktopAlert) (bool, error) {
		close(started)
		select {
		case <-release:
			return false, errors.New("private diagnostic")
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}}
	var logs bytes.Buffer
	delivery := startDesktopAlertDelivery(ctx, store, notifier, log.New(&logs, "", 0))
	defer delivery.stop()
	scheduler.emitDesktopAlert = delivery.enqueue
	check := func() {
		t.Helper()
		if err := scheduler.Trigger(target.ID); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { scheduler.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("notification blocked check completion")
		}
	}
	check()
	check()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("notifier did not receive fault")
	}
	check() // storage and the scheduler slot remain available during notification
	if scheduler.Health(time.Now()).SuccessfulWrites != 3 {
		t.Fatal("expected three committed results")
	}
	stale := desktopAlert{targetID: target.ID, version: target.Version, recovered: true}
	input.Enabled = false
	if _, err := store.SaveTarget(ctx, target.ID, input); err != nil {
		t.Fatal(err)
	}
	for range desktopAlertQueueCapacity + 1 {
		delivery.enqueue(stale)
	}
	if len(delivery.queue) != desktopAlertQueueCapacity || delivery.dropped.Load() != 1 {
		t.Fatal("notification queue did not enforce its capacity")
	}
	close(release)
	// Invalid queued results are consumed without calling the stub again.
	deadline := time.Now().Add(3 * time.Second)
	for len(delivery.queue) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(delivery.queue) > 0 {
		t.Fatal("stale alerts did not clear after the notification call returned")
	}
	delivery.stop()
	if !notifier.closed || !strings.Contains(logs.String(), "提醒提交失败") || strings.Contains(logs.String(), "private diagnostic") {
		t.Fatalf("unexpected notification lifecycle or diagnostics: %q", logs.String())
	}
}

func TestDesktopAlertsUseCommittedCurrentConfiguration(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	input := TargetInput{Name: "example", URL: "http://example.invalid/?token=private", Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5}
	target, err := store.SaveTarget(ctx, 0, input)
	if err != nil {
		t.Fatal(err)
	}
	kind := "timeout"
	var got []desktopAlert
	scheduler, err := NewScheduler(store, 1, func(context.Context, string, time.Duration) Result {
		return Result{Kind: kind, Detail: "private error details", URL: input.URL}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scheduler.Stop)
	scheduler.emitDesktopAlert = func(event desktopAlert) { got = append(got, event) }
	check := func(config Target) {
		t.Helper()
		if err := scheduler.launch(config); err != nil {
			t.Fatal(err)
		}
		scheduler.wg.Wait()
	}
	update := func(enabled bool) Target {
		t.Helper()
		input.Enabled = enabled
		next, err := store.SaveTarget(ctx, target.ID, input)
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	check(target) // first failure
	if _, err := store.db.Exec(`CREATE TRIGGER reject_alert_test BEFORE INSERT ON checks BEGIN SELECT RAISE(ABORT, 'test write failure'); END`); err != nil {
		t.Fatal(err)
	}
	check(target) // failed write must not be counted
	if len(got) != 0 {
		t.Fatalf("a failed write triggered an alert: %+v", got)
	}
	delete(scheduler.retryAt, target.ID)
	if _, err := store.db.Exec(`DROP TRIGGER reject_alert_test`); err != nil {
		t.Fatal(err)
	}
	paused := update(false)
	check(target) // stale result can be written to history only
	check(paused)
	check(paused)
	current := update(true)
	check(current)
	if len(got) != 0 {
		t.Fatalf("paused or old configuration contributed to alert: %+v", got)
	}
	check(current)
	check(current)
	kind = "ok"
	check(current)
	check(current)
	want := []desktopAlert{{eventID: 1, targetID: current.ID, version: current.Version, name: input.Name}, {eventID: 2, targetID: current.ID, version: current.Version, name: input.Name, recovered: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desktop alerts = %+v, want %+v", got, want)
	}
	kind = "timeout"
	check(current)
	if err := store.DeleteTarget(ctx, current.ID); err != nil {
		t.Fatal(err)
	}
	check(current)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deleted result triggered an alert: %+v", got)
	}
}
