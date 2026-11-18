package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

func TestAlertEventMigrationSchema4Rollback(t *testing.T) {
	source, db := backupFixture(t, 4, false)
	if _, err := db.Exec("CREATE TABLE checks_stats(fixture INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenStore(source); err == nil {
		opened.Close()
		t.Fatal("expected migration failure")
	}
	var version, tables int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 4 {
		t.Fatalf("rollback schema %d %v", version, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('alert_events','alert_states')").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("new tables survived rollback %d %v", tables, err)
	}
	if _, err := db.Exec("DROP TABLE checks_stats"); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenStore(source)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	events, err := opened.AlertEvents(context.Background(), 0, 50, 0)
	if err != nil || len(events) != 0 {
		t.Fatal("migration fabricated events")
	}
	// A clean read-only maintenance connection must still accept schema5.
	connection, _, err := openMaintenanceSource(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
}

func queuedEvent(t *testing.T, s *Store, target Target) desktopAlert {
	t.Helper()
	eventCheck(t, s, target, "timeout", "delivery", true)
	e := eventCheck(t, s, target, "timeout", "delivery", true)
	if e == nil {
		t.Fatal("missing fault")
	}
	return desktopAlert{eventID: e.ID, targetID: target.ID, version: target.Version, name: target.Name}
}

func TestAlertEventDeliveryOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		shown        bool
		err          error
	}{
		{"shown", "shown", true, nil}, {"unconfirmed", "submitted_unconfirmed", false, nil}, {"failure", "failed", false, errors.New("secret native diagnostic")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			target, _ := s.SaveTarget(ctx, 0, inputFor("http://example.invalid/"))
			event := queuedEvent(t, s, target)
			var logs bytes.Buffer
			d := &desktopAlertDelivery{ctx: ctx, store: s, logger: log.New(&logs, "", 0)}
			notifier := &desktopNotifierStub{sendEvent: func(context.Context, desktopAlert) (bool, error) {
				events, _ := s.AlertEvents(ctx, 0, 50, 0)
				if events[0].NotificationStatus != "sending" {
					t.Errorf("not claimed before send %+v", events)
				}
				return tc.shown, tc.err
			}}
			d.deliver(s, notifier, event)
			events, _ := s.AlertEvents(ctx, 0, 50, 0)
			if events[0].NotificationStatus != tc.status {
				t.Fatalf("delivery %+v", events)
			}
			b, _ := json.Marshal(events)
			if strings.Contains(string(b), "secret") || strings.Contains(logs.String(), "secret") {
				t.Fatal("raw diagnostic leaked")
			}
		})
	}
}

func TestAlertEventQueueSkipDeleteAndRestart(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	in := inputFor("http://example.invalid/")
	target, _ := s.SaveTarget(ctx, 0, in)
	event := queuedEvent(t, s, target)
	d := &desktopAlertDelivery{ctx: ctx, store: s, queue: make(chan desktopAlert, 1), logger: log.New(&bytes.Buffer{}, "", 0)}
	d.queue <- desktopAlert{}
	d.enqueue(event)
	events, _ := s.AlertEvents(ctx, 0, 50, 0)
	if events[0].NotificationStatus != "queue_full" {
		t.Fatalf("queue full %+v", events)
	}
	eventCheck(t, s, target, "ok", "delivery", true)
	events, _ = s.AlertEvents(ctx, 0, 50, 0)
	event.eventID = events[0].ID
	event.recovered = true
	in.Enabled = false
	s.SaveTarget(ctx, target.ID, in)
	notifier := &desktopNotifierStub{sendEvent: func(context.Context, desktopAlert) (bool, error) { t.Error("stale event sent"); return true, nil }}
	d.deliver(s, notifier, event)
	events, _ = s.AlertEvents(ctx, 0, 50, 0)
	if events[0].NotificationStatus != "skipped" {
		t.Fatalf("skip %+v", events)
	}
	// A new scheduler marks an interrupted queue without replaying it.
	s.db.Exec("UPDATE alert_events SET notification_status='sending' WHERE id=?", event.eventID)
	scheduler, err := NewScheduler(s, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	scheduler.Stop()
	events, _ = s.AlertEvents(ctx, 0, 50, 0)
	if events[0].NotificationStatus != "interrupted" {
		t.Fatalf("restart %+v", events)
	}
	s.DeleteTarget(ctx, target.ID)
	d.deliver(s, notifier, event)
	events, _ = s.AlertEvents(ctx, 0, 50, 0)
	if len(events) != 0 {
		t.Fatal("deleted event revived")
	}
}

func TestAlertEventSchedulerRecordsWithoutDesktopAtCompletion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target, _ := s.SaveTarget(ctx, 0, inputFor("http://example.invalid/"))
	var completed int64
	scheduler, err := NewScheduler(s, 1, func(context.Context, string, time.Duration) Result {
		time.Sleep(20 * time.Millisecond)
		completed = time.Now().UnixMilli()
		return Result{Kind: "timeout"}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	for range 2 {
		if err = scheduler.Trigger(target.ID); err != nil {
			t.Fatal(err)
		}
		scheduler.wg.Wait()
	}
	events, _ := s.AlertEvents(ctx, 0, 50, 0)
	if len(events) != 1 || events[0].NotificationStatus != "disabled" || events[0].OccurredAt < completed {
		t.Fatalf("scheduler events %+v", events)
	}
}

func eventCheck(t *testing.T, s *Store, target Target, kind, session string, desktop bool) *AlertEvent {
	t.Helper()
	e, err := s.recordCheckWithEvents(context.Background(), target, Result{Kind: kind, StatusCode: 503, URL: target.URL, Detail: "secret response token", ErrorCategory: "untrusted private text"}, time.Now(), session, desktop)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAlertEventThresholdAndDisabledNotifications(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target, err := s.SaveTarget(ctx, 0, inputFor("http://example.invalid/?secret=token"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"ok", "timeout", "ok", "timeout"} {
		if e := eventCheck(t, s, target, kind, "first", false); e != nil {
			t.Fatalf("early event %+v", e)
		}
	}
	fault := eventCheck(t, s, target, "timeout", "first", false)
	if fault == nil || fault.Kind != "fault" || fault.NotificationStatus != "disabled" || fault.CheckID <= 0 || fault.OccurredAt < time.Now().Add(-time.Second).UnixMilli() {
		t.Fatalf("fault %+v", fault)
	}
	if eventCheck(t, s, target, "timeout", "first", false) != nil {
		t.Fatal("duplicate fault")
	}
	recovery := eventCheck(t, s, target, "ok", "first", false)
	if recovery == nil || recovery.Kind != "recovery" {
		t.Fatalf("recovery %+v", recovery)
	}
	events, err := s.AlertEvents(ctx, target.ID, 50, 0)
	if err != nil || len(events) != 2 || events[0].ID != recovery.ID || events[1].ID != fault.ID {
		t.Fatalf("events %+v %v", events, err)
	}
	b, _ := json.Marshal(events)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "example.invalid") || strings.Contains(string(b), "untrusted") {
		t.Fatalf("unsafe event output %s", b)
	}
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
		t.Fatalf("schema %d %v", version, err)
	}
}

func TestAlertEventAtomicRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target, _ := s.SaveTarget(ctx, 0, inputFor("http://example.invalid/"))
	eventCheck(t, s, target, "timeout", "run", true)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_event BEFORE INSERT ON alert_events BEGIN SELECT RAISE(ABORT,'test event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.recordCheckWithEvents(ctx, target, Result{Kind: "timeout"}, time.Now(), "run", true); err == nil {
		t.Fatal("expected transaction failure")
	}
	h, err := s.History(ctx, target.ID, 50, 0)
	if err != nil || len(h) != 1 {
		t.Fatalf("check not rolled back %+v %v", h, err)
	}
	var samples int
	if err = s.db.QueryRow("SELECT SUM(samples) FROM daily_summaries").Scan(&samples); err != nil || samples != 1 {
		t.Fatalf("daily rollback %d %v", samples, err)
	}
	s.db.Exec(`DROP TRIGGER fail_event`)
	if e := eventCheck(t, s, target, "timeout", "run", true); e == nil || e.NotificationStatus != "queued" {
		t.Fatalf("state advanced on rollback %+v", e)
	}
}

func TestAlertEventConfigurationSessionAndDeletion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	in := inputFor("http://example.invalid/")
	target, _ := s.SaveTarget(ctx, 0, in)
	eventCheck(t, s, target, "timeout", "a", false)
	if eventCheck(t, s, target, "timeout", "b", false) != nil {
		t.Fatal("restart reused count")
	}
	if eventCheck(t, s, target, "timeout", "b", false) == nil {
		t.Fatal("missing new session fault")
	}
	if eventCheck(t, s, target, "ok", "c", false) != nil {
		t.Fatal("restart fabricated recovery")
	}
	in.Enabled = false
	paused, _ := s.SaveTarget(ctx, target.ID, in)
	eventCheck(t, s, target, "timeout", "c", false)
	eventCheck(t, s, paused, "timeout", "c", false)
	eventCheck(t, s, paused, "timeout", "c", false)
	in.Enabled = true
	current, _ := s.SaveTarget(ctx, target.ID, in)
	if eventCheck(t, s, current, "timeout", "c", false) != nil {
		t.Fatal("pause/edit reused count")
	}
	if eventCheck(t, s, current, "timeout", "c", false) == nil {
		t.Fatal("missing edited config fault")
	}
	events, _ := s.AlertEvents(ctx, 0, 50, 0)
	if len(events) != 2 {
		t.Fatalf("unexpected events %+v", events)
	}
	if err := s.DeleteTarget(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	events, _ = s.AlertEvents(ctx, 0, 50, 0)
	if len(events) != 0 {
		t.Fatal("events survived delete")
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM alert_states").Scan(&n)
	if n != 0 {
		t.Fatal("state survived delete")
	}
}

func TestAlertEventPaginationRetentionAndValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	one, _ := s.SaveTarget(ctx, 0, inputFor("http://example.invalid/one"))
	two, _ := s.SaveTarget(ctx, 0, inputFor("http://example.invalid/two"))
	eventCheck(t, s, two, "timeout", "run", false)
	eventCheck(t, s, two, "timeout", "run", false)
	// Seed a full event history cheaply, then let real transactional insertion prune it.
	_, err := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1001)
 INSERT INTO alert_events(target_id,target_name,config_version,check_id,kind,error_category,occurred_at,notification_status,notification_reason)
 SELECT ?, 'snapshot',1,1,'fault','timeout',?,'disabled','' FROM n`, one.ID, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec("UPDATE alert_events SET occurred_at=? WHERE target_id=? AND id=(SELECT MAX(id) FROM alert_events)", time.Now().Add(-31*24*time.Hour).UnixMilli(), one.ID)
	eventCheck(t, s, one, "timeout", "run", false)
	eventCheck(t, s, one, "timeout", "run", false)
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM alert_events WHERE target_id=?", one.ID).Scan(&n)
	if n != 1000 {
		t.Fatalf("retention %d", n)
	}
	first, err := s.AlertEvents(ctx, one.ID, 50, 0)
	if err != nil || len(first) != 50 {
		t.Fatalf("page %+v %v", first, err)
	}
	second, err := s.AlertEvents(ctx, one.ID, 50, first[49].ID)
	if err != nil || len(second) != 50 || second[0].ID >= first[49].ID {
		t.Fatal("overlap or missing page")
	}
	filtered, _ := s.AlertEvents(ctx, two.ID, 50, 0)
	if len(filtered) != 1 || filtered[0].TargetID != two.ID {
		t.Fatal("target filter")
	}
	for _, p := range [][3]int64{{-1, 50, 0}, {0, 0, 0}, {0, 201, 0}, {0, 50, -1}} {
		if _, err := s.AlertEvents(ctx, p[0], int(p[1]), p[2]); err == nil {
			t.Fatalf("accepted invalid %v", p)
		}
	}
	s.db.Exec("UPDATE alert_events SET occurred_at=?", time.Now().Add(-31*24*time.Hour).UnixMilli())
	expired, _ := s.AlertEvents(ctx, 0, 50, 0)
	if len(expired) != 0 {
		t.Fatal("expired records visible")
	}
}
