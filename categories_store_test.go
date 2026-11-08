package main

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"
)

// This exercises SQL persistence and legacy NULL inference through both readers.
func TestStorePersistsAndInfersErrorCategories(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target, err := s.SaveTarget(ctx, 0, inputFor("http://example.com"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind   string
		status int
		want   string
	}{
		{"ok", 404, ""}, {"request_error", 0, "unknown"}, {"http_error", 404, "http_4xx"}, {"http_error", 503, "http_5xx"},
		{"http_error", 300, "http_other"}, {"assertion_failed", 200, "assertion"}, {"timeout", 0, "timeout"}, {"invalid_input", 0, "input"}, {"body_too_large", 200, "body_too_large"},
	} {
		if _, err = s.db.Exec(`INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at) VALUES(?,?,?, ?,1,?,'old DNS text must not be parsed',?)`, target.ID, target.URL, target.URL, tc.status, tc.kind, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		rows, err := s.History(ctx, target.ID, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := categoryOf(t, rows[0]); got != tc.want {
			t.Fatalf("%s/%d category=%s want%s", tc.kind, tc.status, got, tc.want)
		}
		list, err := s.ListTargets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := categoryOf(t, *list[0].Latest); got != tc.want {
			t.Fatal(got)
		}
	}
	result := Result{Kind: "request_error", ErrorCategory: "dns", URL: target.URL}
	if err = s.RecordCheck(ctx, target, result, time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.History(ctx, target.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := categoryOf(t, rows[0]); got != "dns" {
		t.Fatal(got)
	}
}

func TestStoreErrorCategoriesRetainedFailuresOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	target, err := s.SaveTarget(ctx, 0, inputFor("http://example.com"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind     string
		status   int
		category any
		at       time.Time
	}{
		{"ok", 404, "http_4xx", time.Now()}, {"request_error", 0, "dns", time.Now()}, {"request_error", 0, nil, time.Now()},
		{"request_error", 0, nil, time.Now()}, {"http_error", 500, nil, time.Now()}, {"assertion_failed", 200, nil, time.Now()},
		{"timeout", 0, "timeout", time.Now().Add(-31 * 24 * time.Hour)},
	} {
		if _, err = s.db.Exec(`INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at,error_category) VALUES(?,?,?, ?,1,?,'legacy',?,?)`, target.ID, target.URL, target.URL, tc.status, tc.kind, tc.at.UnixMilli(), tc.category); err != nil {
			t.Fatal(err)
		}
	}
	categories, ok := any(s).(interface {
		ErrorCategories(context.Context, int64) (map[string]int, error)
	})
	if !ok {
		t.Fatal("missing retained error category aggregation")
	}
	got, err := categories.ErrorCategories(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"dns": 1, "unknown": 2, "http_5xx": 1, "assertion": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got%v want%v", got, want)
	}
	if _, err = categories.ErrorCategories(ctx, target.ID+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing target=%v", err)
	}
}

func TestMigrationSchema4AllPathsAndRollback(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			file := legacyDatabase(t)
			db, err := sql.Open("sqlite", file)
			if err != nil {
				t.Fatal(err)
			}
			if version >= 2 {
				if _, err = db.Exec(schemaV2 + "INSERT INTO monitor_metadata VALUES('summary_started_at','123'); PRAGMA user_version=2;"); err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(`INSERT INTO daily_summaries VALUES(7,?,17,12,123,45,0)`, utcDay(time.Now()).Format("2006-01-02")); err != nil {
					t.Fatal(err)
				}
			}
			if version >= 3 {
				if _, err = db.Exec(`ALTER TABLE targets ADD COLUMN rules TEXT NOT NULL DEFAULT '{}'; ALTER TABLE checks ADD COLUMN total_duration_ms REAL; ALTER TABLE checks ADD COLUMN config_version INTEGER; ALTER TABLE checks ADD COLUMN rules_snapshot TEXT; PRAGMA user_version=3;`); err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(`UPDATE targets SET rules='{"body_contains":"fixture"}'; UPDATE checks SET total_duration_ms=123.5,config_version=4,rules_snapshot='{"body_contains":"fixture"}';`); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			for range 2 {
				s, err := OpenStore(file)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
				var actual int
				if err = s.db.QueryRow("PRAGMA user_version").Scan(&actual); err != nil {
					t.Fatal(err)
				}
				if actual != 4 {
					t.Fatalf("version=%d want4", actual)
				}
				history, err := s.History(context.Background(), 7, 10, 0)
				if err != nil || len(history) != 1 || history[0].ID != 11 || categoryOf(t, history[0]) != "" {
					t.Fatalf("history=%+v err=%v", history, err)
				}
				if version < 3 && history[0].TotalDurationMS != nil {
					t.Fatal("legacy total duration fabricated")
				}
				if version == 3 && (history[0].TotalDurationMS == nil || *history[0].TotalDurationMS != 123.5 || history[0].ConfigVersion == nil || *history[0].ConfigVersion != 4 || string(history[0].RulesSnapshot) != `{"body_contains":"fixture"}`) {
					t.Fatalf("snapshot changed: %+v", history[0])
				}
				if version >= 2 {
					var samples, successes int
					var sum, max float64
					if err = s.db.QueryRow("SELECT samples,successes,duration_sum_ms,max_ms FROM daily_summaries WHERE target_id=7").Scan(&samples, &successes, &sum, &max); err != nil || samples != 17 || successes != 12 || sum != 123 || max != 45 {
						t.Fatalf("daily changed: %d/%d/%f/%f %v", samples, successes, sum, max, err)
					}
					var metadata string
					if err = s.db.QueryRow("SELECT value FROM monitor_metadata WHERE key='summary_started_at'").Scan(&metadata); err != nil || metadata != "123" {
						t.Fatalf("metadata changed: %q %v", metadata, err)
					}
				}
				s.Close()
			}
			// Force failure AFTER ALTER and PRAGMA have succeeded, during required-index creation.
			db, err = sql.Open("sqlite", file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("ALTER TABLE checks DROP COLUMN error_category; PRAGMA user_version=3; DROP INDEX checks_stats; CREATE TABLE checks_stats(fixture INTEGER);"); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if s, err := OpenStore(file); err == nil {
				s.Close()
				t.Fatal("expected late migration failure")
			}
			db, err = sql.Open("sqlite", file)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var actual, count int
			db.QueryRow("PRAGMA user_version").Scan(&actual)
			if actual != 3 {
				t.Fatalf("failed migration committed version%d", actual)
			}
			if err = db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('checks') WHERE name='error_category'").Scan(&count); err != nil || count != 0 {
				t.Fatalf("ALTER survived rollback: %d %v", count, err)
			}
			db.QueryRow("SELECT COUNT(*) FROM checks WHERE id=11").Scan(&count)
			if count != 1 {
				t.Fatal("history lost")
			}
		})
	}
}

// A conflicting index name fails at the final migration operation. Every earlier
// schema change, not just user_version, must be rolled back from each old schema.
func TestMigrationLateFailureRollsBackFromV1AndV2(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			file := legacyDatabase(t)
			db, err := sql.Open("sqlite", file)
			if err != nil {
				t.Fatal(err)
			}
			if version == 2 {
				if _, err = db.Exec(schemaV2 + "INSERT INTO monitor_metadata VALUES('summary_started_at','123');PRAGMA user_version=2;"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = db.Exec("CREATE TABLE checks_stats(fixture INTEGER)"); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if s, err := OpenStore(file); err == nil {
				s.Close()
				t.Fatal("expected late failure")
			}
			db, err = sql.Open("sqlite", file)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var actual int
			if err = db.QueryRow("PRAGMA user_version").Scan(&actual); err != nil || actual != version {
				t.Fatalf("version=%d err=%v", actual, err)
			}
			for _, name := range []string{"total_duration_ms", "config_version", "rules_snapshot", "error_category"} {
				var count int
				if err = db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('checks') WHERE name=?", name).Scan(&count); err != nil || count != 0 {
					t.Fatalf("partial column %s committed: count%d err%v", name, count, err)
				}
			}
			var count int
			if err = db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('targets') WHERE name='rules'").Scan(&count); err != nil || count != 0 {
				t.Fatalf("rules column survived rollback: %d %v", count, err)
			}
			if version == 1 {
				if err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('daily_summaries','monitor_metadata')").Scan(&count); err != nil || count != 0 {
					t.Fatalf("summary tables survived rollback: %d %v", count, err)
				}
			}
			if err = db.QueryRow("SELECT COUNT(*) FROM checks WHERE id=11").Scan(&count); err != nil || count != 1 {
				t.Fatalf("history changed: %d %v", count, err)
			}
		})
	}
}
