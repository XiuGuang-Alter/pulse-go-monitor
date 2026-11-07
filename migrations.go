package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const schemaV1 = `CREATE TABLE IF NOT EXISTS targets (
 id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, url TEXT NOT NULL,
 interval_seconds INTEGER NOT NULL, timeout_seconds INTEGER NOT NULL,
 enabled INTEGER NOT NULL, created_at INTEGER NOT NULL, next_check_at INTEGER NOT NULL,
 version INTEGER NOT NULL DEFAULT 1);
CREATE TABLE IF NOT EXISTS checks (
 id INTEGER PRIMARY KEY AUTOINCREMENT, target_id INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
 input_url TEXT NOT NULL, final_url TEXT NOT NULL, status_code INTEGER NOT NULL,
 duration_ms REAL NOT NULL, kind TEXT NOT NULL, detail TEXT NOT NULL, checked_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS checks_target_id ON checks(target_id,id DESC);`

const schemaV2 = `CREATE TABLE daily_summaries (
 target_id INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
 date TEXT NOT NULL, samples INTEGER NOT NULL, successes INTEGER NOT NULL,
 duration_sum_ms REAL NOT NULL, max_ms REAL NOT NULL,
 migration_partial INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(target_id,date));
CREATE INDEX daily_date ON daily_summaries(date);
CREATE TABLE monitor_metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);`

func migrateStore(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 3 {
		return fmt.Errorf("数据库版本%d比程序支持的版本新", version)
	}
	now := time.Now().UTC()
	if version == 0 {
		if _, err = tx.ExecContext(ctx, schemaV1); err != nil {
			return err
		}
		version = 1
	}
	if version == 1 {
		if _, err = tx.ExecContext(ctx, schemaV2); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO monitor_metadata VALUES('summary_started_at',?)", now.UnixMilli()); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO daily_summaries(target_id,date,samples,successes,duration_sum_ms,max_ms,migration_partial)
   SELECT target_id,date(checked_at/1000.0,'unixepoch'),COUNT(*),SUM(CASE WHEN kind='ok' THEN 1 ELSE 0 END),SUM(duration_ms),MAX(duration_ms),1
   FROM checks WHERE checked_at>=? AND checked_at<? GROUP BY target_id,date(checked_at/1000.0,'unixepoch')`, utcDay(now).AddDate(0, 0, -29).UnixMilli(), utcDay(now).AddDate(0, 0, 1).UnixMilli()); err != nil {
			return err
		}
		version = 2
	}
	if version == 2 {
		if _, err = tx.ExecContext(ctx, `ALTER TABLE targets ADD COLUMN rules TEXT NOT NULL DEFAULT '{}';
   ALTER TABLE checks ADD COLUMN total_duration_ms REAL;
   ALTER TABLE checks ADD COLUMN config_version INTEGER;
   ALTER TABLE checks ADD COLUMN rules_snapshot TEXT;`); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "PRAGMA user_version=3"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS checks_target_id ON checks(target_id,id DESC); CREATE INDEX IF NOT EXISTS checks_stats ON checks(target_id,checked_at,kind,duration_ms)"); err != nil {
		return err
	}
	return tx.Commit()
}
func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
