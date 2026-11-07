package main

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationVersion2AndFuture(t *testing.T) {
	file := legacyDatabase(t)
	db, _ := sql.Open("sqlite", file)
	if _, err := db.Exec(schemaV2 + "INSERT INTO monitor_metadata VALUES('summary_started_at','123');PRAGMA user_version=2;"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	s.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != 3 {
		t.Fatal(v)
	}
	s.Close()
	db, _ = sql.Open("sqlite", file)
	db.Exec("PRAGMA user_version=4")
	db.Close()
	if s, err := OpenStore(file); err == nil {
		s.Close()
		t.Fatal("future version accepted")
	}
}
func TestDailyAPI(t *testing.T) {
	s := testStore(t)
	target, _ := s.SaveTarget(context.Background(), 0, inputFor("http://example.com"))
	s.RecordCheck(context.Background(), target, Result{Kind: "ok"}, time.Now())
	scheduler, _ := NewScheduler(s, 1, nil)
	defer scheduler.Stop()
	handler := NewApp(s, scheduler).Handler()
	for _, tc := range []struct {
		path string
		want int
	}{{"/api/targets/1/daily", 200}, {"/api/targets/1/daily?days=1", 200}, {"/api/targets/1/daily?days=31", 400}, {"/api/targets/1/daily?days=abc", 400}, {"/api/targets/999/daily", 404}} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:8080"+tc.path, nil))
		if w.Code != tc.want {
			t.Errorf("%s: %d want%d", tc.path, w.Code, tc.want)
		}
	}
}

const legacySchema = `CREATE TABLE targets(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,url TEXT NOT NULL,interval_seconds INTEGER NOT NULL,timeout_seconds INTEGER NOT NULL,enabled INTEGER NOT NULL,created_at INTEGER NOT NULL,next_check_at INTEGER NOT NULL,version INTEGER NOT NULL DEFAULT 1);
CREATE TABLE checks(id INTEGER PRIMARY KEY AUTOINCREMENT,target_id INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,input_url TEXT NOT NULL,final_url TEXT NOT NULL,status_code INTEGER NOT NULL,duration_ms REAL NOT NULL,kind TEXT NOT NULL,detail TEXT NOT NULL,checked_at INTEGER NOT NULL);
CREATE INDEX checks_target_id ON checks(target_id,id DESC);PRAGMA user_version=1;`

func legacyDatabase(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err = db.Exec(`INSERT INTO targets VALUES(7,'legacy','http://example.com',60,1,0,?,?,4); INSERT INTO checks VALUES(11,7,'http://example.com','http://example.com',200,12,'ok','legacy',?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	return file
}
func TestMigrationLegacyAndRollback(t *testing.T) {
	file := legacyDatabase(t)
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	var version int
	s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 3 {
		t.Fatalf("version=%d want3", version)
	}
	rows, err := s.History(context.Background(), 7, 10, 0)
	if err != nil || len(rows) != 1 || rows[0].ID != 11 {
		t.Fatal(rows, err)
	}
	daily, err := s.Daily(context.Background(), 7, 1)
	if err != nil || daily.Buckets[0].Samples != 1 || !daily.Buckets[0].MigrationPartial {
		t.Fatal(daily, err)
	}
	s.Close()
	s, err = OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	daily, _ = s.Daily(context.Background(), 7, 1)
	if daily.Buckets[0].Samples != 1 {
		t.Fatal("migration repeated")
	}
	bad := legacyDatabase(t)
	db, _ := sql.Open("sqlite", bad)
	db.Exec(`ALTER TABLE targets ADD COLUMN rules TEXT NOT NULL DEFAULT '{}'`)
	db.Close()
	if failed, err := OpenStore(bad); err == nil {
		failed.Close()
		t.Fatal("expected migration failure")
	}
	db, _ = sql.Open("sqlite", bad)
	defer db.Close()
	db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 1 {
		t.Fatal("failed migration changed version")
	}
	var count int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='daily_summaries'").Scan(&count)
	if count != 0 {
		t.Fatal("partial migration committed")
	}
	db.QueryRow("SELECT COUNT(*) FROM checks WHERE id=11").Scan(&count)
	if count != 1 {
		t.Fatal("legacy data lost")
	}
}
func TestDailyRetentionAtomicityAndUTCDays(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	target, _ := s.SaveTarget(ctx, 0, inputFor("http://example.com"))
	today := time.Now().UTC().Truncate(24 * time.Hour)
	record := func(at time.Time) {
		t.Helper()
		if err := s.RecordCheck(ctx, target, Result{Kind: "ok", Duration: 10 * time.Millisecond}, at); err != nil {
			t.Fatal(err)
		}
	}
	record(today.Add(-time.Millisecond))
	for i := 0; i < 1002; i++ {
		record(today.Add(time.Millisecond))
	}
	daily, err := s.Daily(ctx, target.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if daily.Timezone != "UTC" || daily.Buckets[0].Availability != nil || daily.Buckets[0].AverageMS != nil || daily.Buckets[1].Samples != 1 || daily.Buckets[2].Samples != 1002 || !daily.Buckets[2].InProgress {
		t.Fatal(daily)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM checks").Scan(&n)
	if n != 1000 {
		t.Fatal(n)
	}
	record(today.AddDate(0, 0, -30))
	daily, _ = s.Daily(ctx, target.ID, 30)
	for _, b := range daily.Buckets {
		if b.Date == today.AddDate(0, 0, -30).Format("2006-01-02") {
			t.Fatal("expired day revived")
		}
	}
	if _, err = s.Daily(ctx, target.ID, 0); err == nil {
		t.Fatal("invalid range accepted")
	}
	s.db.Exec(`CREATE TRIGGER reject_day BEFORE INSERT ON daily_summaries BEGIN SELECT RAISE(ABORT,'rollback'); END`)
	if err = s.RecordCheck(ctx, target, Result{Kind: "ok"}, time.Now()); err == nil {
		t.Fatal("expected transaction failure")
	}
	daily, _ = s.Daily(ctx, target.ID, 1)
	if daily.Buckets[0].Samples != 1002 {
		t.Fatal("failed transaction changed summary")
	}
	s.db.Exec("DROP TRIGGER reject_day")
	s.DeleteTarget(ctx, target.ID)
	s.db.QueryRow("SELECT COUNT(*) FROM daily_summaries").Scan(&n)
	if n != 0 {
		t.Fatal("daily cascade failed")
	}
}
