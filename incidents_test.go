package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIncidentAPIAndRetention(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	current, err := s.SaveTarget(ctx, 0, inputFor("http://example.invalid/?token=private"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.SaveTarget(ctx, 0, inputFor("http://example.invalid/other"))
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(s, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	start := time.Now().Add(-31 * 24 * time.Hour)
	saveIncidentSample(t, s, current, "timeout", scheduler.instanceID, start)
	saveIncidentSample(t, s, current, "timeout", scheduler.instanceID, start.Add(time.Second))
	// A backward clock step makes the time span uncertain even if the clock later catches up.
	saveIncidentSample(t, s, current, "timeout", scheduler.instanceID, start)
	saveIncidentSample(t, s, current, "timeout", scheduler.instanceID, time.Now())
	for _, kind := range []string{"http_error", "http_error", "ok"} {
		saveIncidentSample(t, s, other, kind, scheduler.instanceID, time.Now())
	}
	handler := NewApp(s, scheduler).Handler()
	call := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1/api/incidents"+query, nil))
		return rec
	}
	read := func(query string) []Incident {
		t.Helper()
		rec := call(query)
		var page struct {
			Items      []Incident
			ObservedAt int64 `json:"observed_at"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil || page.ObservedAt <= 0 || strings.Contains(rec.Body.String(), "private") {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		return page.Items
	}
	if rows := read("?limit=1"); len(rows) != 1 || rows[0].Status != "recovered" {
		t.Fatalf("first page %+v", rows)
	}
	if rows := read("?limit=1&before_id=2&status=active&target_id=1"); len(rows) != 1 || rows[0].TargetID != current.ID || rows[0].ObservedDurationMS != nil {
		t.Fatalf("filtered old active %+v", rows)
	}
	if rows := read("?target_id=2&status=active"); len(rows) != 0 {
		t.Fatalf("unexpected match %+v", rows)
	}
	for _, query := range []string{"?status=bogus", "?status=active&status=recovered", "?limit=0", "?target_id=-1", "?before_id=bad"} {
		if rec := call(query); rec.Code != 400 {
			t.Fatalf("%s: %d", query, rec.Code)
		}
	}
	if rec := call("?target_id=987"); rec.Code != 404 {
		t.Fatalf("unknown target %d", rec.Code)
	}
	now := time.Now()
	_, err = s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1001)
	 INSERT INTO incidents(target_id,target_name,config_version,session,status,started_at,confirmed_at,last_failure_at,
	 first_failure_check_id,last_failure_check_id,fault_event_id,error_category,failure_count,closed_at)
	 SELECT ?, 'fixture',1,'fixture','interrupted',1,2,2,1,2,1,'timeout',2,? FROM n`, current.ID, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if err = pruneIncidents(ctx, s.db, 0, now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM incidents WHERE target_id=?", current.ID).Scan(&count); err != nil || count != 1001 {
		t.Fatalf("retention %d %v", count, err)
	}
	if _, err = s.db.Exec("UPDATE incidents SET closed_at=? WHERE closed_at IS NOT NULL", start.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if rows := read(""); len(rows) != 1 || rows[0].Status != "active" {
		t.Fatalf("expired closed rows visible %+v", rows)
	}
	saveIncidentSample(t, s, current, "timeout", scheduler.instanceID, time.Now())
	if err = s.db.QueryRow("SELECT COUNT(*) FROM incidents").Scan(&count); err != nil || count != 1 {
		t.Fatalf("normal sampling did not physically remove expired history: %d %v", count, err)
	}
}

// Save checks through the production transaction, injecting only its observation clock.
func incidentSample(t *testing.T, s *Store, target Target, kind, session string, at time.Time) (*AlertEvent, error) {
	t.Helper()
	options := &alertCheckOptions{session: session, completedAt: at}
	_, err := s.recordCheck(context.Background(), target, Result{Kind: kind, Detail: "private", URL: target.URL}, at.Add(-time.Second), options)
	return options.event, err
}

func saveIncidentSample(t *testing.T, s *Store, target Target, kind, session string, at time.Time) *AlertEvent {
	t.Helper()
	event, err := incidentSample(t, s, target, kind, session, at)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func incidentList(t *testing.T, s *Store) []Incident {
	t.Helper()
	rows, err := s.Incidents(context.Background(), 0, "", 50, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestIncidentLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target, err := s.SaveTarget(ctx, 0, inputFor("http://example.invalid/?token=private"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	write := func(kind string, minute int) *AlertEvent {
		return saveIncidentSample(t, s, target, kind, "run", start.Add(time.Duration(minute)*time.Minute))
	}
	write("timeout", 0)
	write("ok", 1)
	if len(incidentList(t, s)) != 0 {
		t.Fatal("isolated failure created an incident")
	}
	write("timeout", 2)
	if len(incidentList(t, s)) != 0 {
		t.Fatal("unconfirmed incident is visible")
	}
	fault := write("timeout", 3)
	write("timeout", 4)
	rows := incidentList(t, s)
	if fault == nil || len(rows) != 1 {
		t.Fatalf("fault=%+v rows=%+v", fault, rows)
	}
	active := rows[0]
	if active.Status != "active" || active.StartedAt != start.Add(2*time.Minute).UnixMilli() || active.ConfirmedAt != fault.OccurredAt ||
		active.FailureCount != 3 || active.FirstFailureCheckID != fault.CheckID-1 || active.FaultEventID != fault.ID ||
		active.ObservedDurationMS == nil || *active.ObservedDurationMS != 120000 || active.RecoveryDurationMS != nil {
		t.Fatalf("active %+v", active)
	}
	if _, err = s.db.Exec("DELETE FROM checks; DELETE FROM alert_events"); err != nil {
		t.Fatal(err)
	}
	recovery := write("ok", 6)
	write("ok", 7)
	rows = incidentList(t, s)
	if recovery == nil || len(rows) != 1 {
		t.Fatalf("recovery=%+v rows=%+v", recovery, rows)
	}
	done := rows[0]
	if done.ID != active.ID || done.Status != "recovered" || done.RecoveredAt == nil || *done.RecoveredAt != recovery.OccurredAt ||
		done.RecoveryEventID == nil || *done.RecoveryEventID != recovery.ID || done.RecoveryDurationMS == nil || *done.RecoveryDurationMS != 240000 {
		t.Fatalf("recovered %+v", done)
	}
}

func TestIncidentRollbackAndInterrupt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	in := inputFor("http://example.invalid/")
	target, err := s.SaveTarget(ctx, 0, in)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Minute)
	saveIncidentSample(t, s, target, "timeout", "a", start)
	exec := func(query string) {
		t.Helper()
		if _, err := s.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TRIGGER reject_incident BEFORE INSERT ON incidents BEGIN SELECT RAISE(ABORT,'test'); END`)
	if event, err := incidentSample(t, s, target, "timeout", "a", start.Add(time.Second)); err == nil || event != nil {
		t.Fatal("incident failure committed")
	}
	history, err := s.History(ctx, target.ID, 50, 0)
	events, eventErr := s.AlertEvents(ctx, 0, 50, 0)
	if err != nil || eventErr != nil || len(history) != 1 || len(events) != 0 {
		t.Fatalf("rollback history=%v events=%v errors=%v/%v", history, events, err, eventErr)
	}
	exec("DROP TRIGGER reject_incident")
	saveIncidentSample(t, s, target, "timeout", "a", start.Add(2*time.Second))
	in.Enabled = false
	exec(`CREATE TRIGGER reject_close BEFORE UPDATE ON incidents BEGIN SELECT RAISE(ABORT,'test'); END`)
	if _, err = s.SaveTarget(ctx, target.ID, in); err == nil {
		t.Fatal("pause succeeded with failed incident update")
	}
	current, err := s.GetTarget(ctx, target.ID)
	if err != nil || !current.Enabled || current.Version != target.Version {
		t.Fatalf("target update was not rolled back %+v %v", current, err)
	}
	if event, err := incidentSample(t, s, target, "ok", "a", start.Add(3*time.Second)); err == nil || event != nil {
		t.Fatal("recovery escaped rollback")
	}
	exec("DROP TRIGGER reject_close")
	paused, err := s.SaveTarget(ctx, target.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	saveIncidentSample(t, s, target, "ok", "a", start.Add(4*time.Second))
	saveIncidentSample(t, s, paused, "timeout", "a", start.Add(5*time.Second))
	rows := incidentList(t, s)
	if len(rows) != 1 || rows[0].Status != "interrupted" || rows[0].InterruptReason != "paused" || rows[0].RecoveredAt != nil || rows[0].RecoveryDurationMS != nil {
		t.Fatalf("paused %+v", rows)
	}
	if err = s.DeleteTarget(ctx, target.ID); err != nil || len(incidentList(t, s)) != 0 {
		t.Fatalf("delete %v", err)
	}
}

func TestIncidentShutdownAndRestart(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target, err := s.SaveTarget(ctx, 0, inputFor("http://example.invalid/"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewScheduler(s, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	saveIncidentSample(t, s, target, "timeout", a.instanceID, time.Now())
	saveIncidentSample(t, s, target, "timeout", a.instanceID, time.Now())
	a.Stop()
	if rows := incidentList(t, s); len(rows) != 1 || rows[0].InterruptReason != "stopped" {
		t.Fatalf("shutdown %+v", rows)
	}
	saveIncidentSample(t, s, target, "timeout", "terminated", time.Now())
	saveIncidentSample(t, s, target, "timeout", "terminated", time.Now())
	b, err := NewScheduler(s, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	saveIncidentSample(t, s, target, "ok", b.instanceID, time.Now())
	rows := incidentList(t, s)
	if len(rows) != 2 || rows[0].InterruptReason != "restarted" || rows[0].RecoveredAt != nil {
		t.Fatalf("restart %+v", rows)
	}
	var pending int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM alert_states").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("stale state %d %v", pending, err)
	}
}

func TestIncidentShutdownLetsInProgressSaveCommit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target, err := s.SaveTarget(ctx, 0, inputFor("http://example.invalid/"))
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(s, 1, func(context.Context, string, time.Duration) Result {
		return Result{Kind: "ok"}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	saveIncidentSample(t, s, target, "timeout", scheduler.instanceID, time.Now())
	saveIncidentSample(t, s, target, "timeout", scheduler.instanceID, time.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	waiters := s.db.Stats().WaitCount
	if err = scheduler.launch(target); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return s.db.Stats().WaitCount > waiters })
	stopped := make(chan struct{})
	go func() {
		scheduler.Stop()
		close(stopped)
	}()
	<-scheduler.ctx.Done()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(6 * time.Second):
		t.Fatal("stop did not complete")
	}
	items := incidentList(t, s)
	if len(items) != 1 || items[0].Status != "recovered" {
		t.Fatalf("normal shutdown lost a recovery already in storage: %+v", items)
	}
}
