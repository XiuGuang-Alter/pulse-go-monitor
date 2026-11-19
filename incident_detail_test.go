package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func readIncidentDetail(t *testing.T, handler http.Handler, id string, wantCode int) []byte {
	t.Helper()
	r := httptest.NewRecorder()
	handler.ServeHTTP(r, httptest.NewRequest("GET", "http://127.0.0.1/api/incidents/"+id+"/checks", nil))
	if r.Code != wantCode {
		t.Fatalf("incident %s: want %d, got %d: %s", id, wantCode, r.Code, r.Body.String())
	}
	if wantCode == 404 && !strings.Contains(r.Body.String(), "故障记录已不存在") {
		t.Fatalf("wrong missing-incident error: %s", r.Body.String())
	}
	return r.Body.Bytes()
}

func TestIncidentDetailSnapshot(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	app := NewApp(s, nil).Handler()
	target, err := s.SaveTarget(ctx, 0, inputFor("http://a.invalid/?token=private"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.SaveTarget(ctx, 0, inputFor("http://b.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	old := target
	target, err = s.SaveTarget(ctx, target.ID, target.TargetInput)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	write := func(target Target, kind string, status int) {
		t.Helper()
		_, err := s.recordCheck(ctx, target, Result{Kind: kind, StatusCode: status, Detail: "private"}, now, &alertCheckOptions{session: "fixture", completedAt: now})
		if err != nil {
			t.Fatal(err)
		}
	}
	write(target, "http_error", 503)
	write(other, "timeout", 0)
	write(old, "timeout", 0)
	write(target, "timeout", 0)
	now = now.Add(-time.Second) // A backwards clock must not reorder an incident.
	write(target, "assertion_failed", 200)
	check := func(id, status string, total, kept int) []CheckRecord {
		t.Helper()
		body := readIncidentDetail(t, app, id, 200)
		if strings.Contains(string(body), "private") || strings.Contains(string(body), "rules_snapshot") || strings.Contains(string(body), "input_url") {
			t.Fatalf("unexpected data: %s", body)
		}
		var detail struct {
			Status                   string
			Total, Retained, Cleared int
			Items                    []CheckRecord
		}
		if err := json.Unmarshal(body, &detail); err != nil {
			t.Fatal(err)
		}
		if detail.Status != status || detail.Total != total || detail.Retained != kept || detail.Cleared != total-kept || detail.Items == nil || len(detail.Items) != kept {
			t.Fatalf("bad snapshot: %s", body)
		}
		return detail.Items
	}
	items := check("1", "active", 3, 3)
	if items[0].StatusCode != 503 || items[0].ErrorCategory != "http_5xx" ||
		items[1].StatusCode != 0 || items[1].ErrorCategory != "timeout" ||
		items[2].CheckedAt >= items[1].CheckedAt || items[2].StatusCode != 200 ||
		items[2].Kind != "assertion_failed" || items[2].Detail != safeDetail("assertion_failed", 200) {
		t.Fatalf("mixed or incorrect checks: %+v", items)
	}
	write(target, "ok", 200)
	write(target, "timeout", 0)
	write(target, "timeout", 0)
	if err = s.stopIncidents(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	items = check("1", "recovered", 4, 4)
	if items[3].ID != 6 || items[3].Kind != "ok" || items[3].Detail != safeDetail("ok", 200) {
		t.Fatalf("wrong recovery %+v", items)
	}
	check("2", "interrupted", 2, 2)
	if _, err = s.db.Exec("UPDATE checks SET checked_at=? WHERE id IN (1,4)", now.Add(-31*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	check("1", "recovered", 4, 2)
	if _, err = s.db.Exec("DELETE FROM checks WHERE target_id=?", target.ID); err != nil {
		t.Fatal(err)
	}
	check("1", "recovered", 4, 0)
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
		t.Fatalf("schema %d, %v", version, err)
	}
	if _, err = s.db.Exec("UPDATE incidents SET closed_at=? WHERE id=1", now.Add(-31*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	readIncidentDetail(t, app, "1", 404)
	if err = s.DeleteTarget(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	readIncidentDetail(t, app, "2", 404)
	for _, bad := range []string{"0", "-1", "abc", "18446744073709551616"} {
		readIncidentDetail(t, app, bad, 400)
	}
}

func TestIncidentDetailThousandCheckLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target, err := s.SaveTarget(ctx, 0, inputFor("http://a.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	saveIncidentSample(t, s, target, "timeout", "fixture", now)
	saveIncidentSample(t, s, target, "timeout", "fixture", now)
	// Bulk-seed the middle of one long incident, then exercise normal write-side eviction.
	if _, err = s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<999)
	INSERT INTO checks(target_id,config_version,input_url,final_url,status_code,duration_ms,kind,detail,checked_at)
	SELECT ?,?, '', '',0,1,'timeout','',? FROM n`, target.ID, target.Version, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE incidents SET failure_count=1001,last_failure_check_id=(SELECT max(id) FROM checks)"); err != nil {
		t.Fatal(err)
	}
	saveIncidentSample(t, s, target, "ok", "fixture", now)
	var result struct {
		Total, Retained, Cleared int
		Items                    []CheckRecord
	}
	if err = json.Unmarshal(readIncidentDetail(t, NewApp(s, nil).Handler(), "1", 200), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 1002 || result.Retained != 1000 || result.Cleared != 2 || len(result.Items) != 1000 ||
		result.Items[0].ID != 3 || result.Items[999].ID != 1002 || result.Items[999].Kind != "ok" {
		t.Fatalf("bad retained range, total=%d retained=%d cleared=%d", result.Total, result.Retained, result.Cleared)
	}
}
