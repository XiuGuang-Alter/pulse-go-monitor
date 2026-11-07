package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("数据库路径不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	fail := func(err error) (*Store, error) { db.Close(); return nil, err }
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version > 1 {
		return fail(fmt.Errorf("数据库版本%d比程序支持的版本新", version))
	}
	_, err = db.Exec(`PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;
 CREATE TABLE IF NOT EXISTS targets (
 id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, url TEXT NOT NULL,
 interval_seconds INTEGER NOT NULL, timeout_seconds INTEGER NOT NULL,
 enabled INTEGER NOT NULL, created_at INTEGER NOT NULL, next_check_at INTEGER NOT NULL,
 version INTEGER NOT NULL DEFAULT 1);
 CREATE TABLE IF NOT EXISTS checks (
 id INTEGER PRIMARY KEY AUTOINCREMENT, target_id INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
 input_url TEXT NOT NULL, final_url TEXT NOT NULL, status_code INTEGER NOT NULL,
 duration_ms REAL NOT NULL, kind TEXT NOT NULL, detail TEXT NOT NULL, checked_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS checks_target_id ON checks(target_id,id DESC);
 PRAGMA user_version=1;`)
	if err != nil {
		return fail(err)
	}
	if _, err = db.Exec("DELETE FROM checks WHERE checked_at<?", time.Now().Add(-30*24*time.Hour).UnixMilli()); err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

const targetColumns = `id,name,url,interval_seconds,timeout_seconds,enabled,created_at,next_check_at,version`

type rowScanner interface{ Scan(...any) error }

func scanTarget(row rowScanner) (Target, error) {
	var t Target
	err := row.Scan(&t.ID, &t.Name, &t.URL, &t.IntervalSeconds, &t.TimeoutSeconds, &t.Enabled, &t.CreatedAt, &t.NextCheckAt, &t.Version)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return t, err
}
func (s *Store) GetTarget(ctx context.Context, id int64) (Target, error) {
	return scanTarget(s.db.QueryRowContext(ctx, "SELECT "+targetColumns+" FROM targets WHERE id=?", id))
}
func (s *Store) SaveTarget(ctx context.Context, id int64, in TargetInput) (Target, error) {
	if err := validateTarget(&in); err != nil {
		return Target{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Target{}, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if id == 0 {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM targets").Scan(&count); err != nil {
			return Target{}, err
		}
		if count >= 100 {
			return Target{}, ErrLimit
		}
		result, e := tx.ExecContext(ctx, `INSERT INTO targets(name,url,interval_seconds,timeout_seconds,enabled,created_at,next_check_at) VALUES(?,?,?,?,?,?,?)`, in.Name, in.URL, in.IntervalSeconds, in.TimeoutSeconds, in.Enabled, now, now)
		if e != nil {
			return Target{}, e
		}
		id, err = result.LastInsertId()
		if err != nil {
			return Target{}, err
		}
	} else {
		result, e := tx.ExecContext(ctx, `UPDATE targets SET name=?,url=?,interval_seconds=?,timeout_seconds=?,enabled=?,next_check_at=?,version=version+1 WHERE id=?`, in.Name, in.URL, in.IntervalSeconds, in.TimeoutSeconds, in.Enabled, now, id)
		if e != nil {
			return Target{}, e
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return Target{}, ErrNotFound
		}
	}
	if err = tx.Commit(); err != nil {
		return Target{}, err
	}
	return s.GetTarget(ctx, id)
}
func (s *Store) DeleteTarget(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM targets WHERE id=?", id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Store) targets(ctx context.Context, where string, args ...any) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+targetColumns+" FROM targets "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Target{}
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) DueTargets(ctx context.Context) ([]Target, error) {
	return s.targets(ctx, "WHERE enabled=1 AND next_check_at<=? ORDER BY next_check_at,id", time.Now().UnixMilli())
}
func (s *Store) ListTargets(ctx context.Context) ([]Target, error) {
	targets, err := s.targets(ctx, "ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	for i := range targets {
		t := &targets[i]
		latest, err := s.history(ctx, t.ID, 1, 0)
		if err != nil {
			return nil, err
		}
		if len(latest) > 0 {
			t.Latest = &latest[0]
		}
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN kind='ok' THEN 1 ELSE 0 END),0),COALESCE(AVG(duration_ms),0) FROM checks WHERE target_id=? AND checked_at>=?`, t.ID, time.Now().Add(-30*24*time.Hour).UnixMilli()).Scan(&t.Stats.Samples, &t.Stats.Successes, &t.Stats.AverageMS)
		if err != nil {
			return nil, err
		}
		if t.Stats.Samples > 0 {
			t.Stats.Availability = 100 * float64(t.Stats.Successes) / float64(t.Stats.Samples)
		}
	}
	return targets, nil
}
func (s *Store) RecordCheck(ctx context.Context, t Target, r Result, checkedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT 1 FROM targets WHERE id=?", t.ID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at) VALUES(?,?,?,?,?,?,?,?)`, t.ID, t.URL, r.URL, r.StatusCode, float64(r.Duration)/float64(time.Millisecond), r.Kind, r.Detail, checkedAt.UnixMilli())
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM checks WHERE target_id=? AND (checked_at<? OR id NOT IN (SELECT id FROM checks WHERE target_id=? ORDER BY id DESC LIMIT 1000))`, t.ID, time.Now().Add(-30*24*time.Hour).UnixMilli(), t.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE targets SET next_check_at=? WHERE id=? AND version=?`, time.Now().Add(time.Duration(t.IntervalSeconds)*time.Second).UnixMilli(), t.ID, t.Version)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) History(ctx context.Context, id int64, limit int, before int64) ([]CheckRecord, error) {
	if limit < 1 || limit > 200 || before < 0 {
		return nil, fmt.Errorf("%w：limit为1–200，before_id不可为负", ErrValidation)
	}
	if _, err := s.GetTarget(ctx, id); err != nil {
		return nil, err
	}
	return s.history(ctx, id, limit, before)
}
func (s *Store) history(ctx context.Context, id int64, limit int, before int64) ([]CheckRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at FROM checks WHERE target_id=? AND checked_at>=? AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, id, time.Now().Add(-30*24*time.Hour).UnixMilli(), before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckRecord{}
	for rows.Next() {
		var r CheckRecord
		if err = rows.Scan(&r.ID, &r.TargetID, &r.InputURL, &r.FinalURL, &r.StatusCode, &r.DurationMS, &r.Kind, &r.Detail, &r.CheckedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
