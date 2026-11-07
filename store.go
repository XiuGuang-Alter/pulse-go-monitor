package main

import (
	"context"
	"database/sql"
	"encoding/json"
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
	_, err = db.Exec(`PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;`)
	if err != nil {
		return fail(err)
	}
	if err = migrateStore(context.Background(), db); err != nil {
		return fail(err)
	}
	if _, err = db.Exec("DELETE FROM checks WHERE checked_at<?", time.Now().Add(-30*24*time.Hour).UnixMilli()); err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

const targetColumns = `id,name,url,interval_seconds,timeout_seconds,enabled,created_at,next_check_at,version,rules`
const checkColumns = `id,target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at,total_duration_ms,config_version,rules_snapshot`

type rowScanner interface{ Scan(...any) error }

func scanTarget(row rowScanner) (Target, error) {
	var t Target
	var rules string
	err := row.Scan(&t.ID, &t.Name, &t.URL, &t.IntervalSeconds, &t.TimeoutSeconds, &t.Enabled, &t.CreatedAt, &t.NextCheckAt, &t.Version, &rules)
	t.Rules = json.RawMessage(rules)
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
	if id != 0 && len(in.Rules) == 0 {
		var previous string
		if err = tx.QueryRowContext(ctx, "SELECT rules FROM targets WHERE id=?", id).Scan(&previous); errors.Is(err, sql.ErrNoRows) {
			return Target{}, ErrNotFound
		} else if err != nil {
			return Target{}, err
		}
		in.Rules = json.RawMessage(previous)
	}
	rules, _ := parseRules(in.Rules)
	in.Rules, _ = json.Marshal(rules)
	now := time.Now().UnixMilli()
	if id == 0 {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM targets").Scan(&count); err != nil {
			return Target{}, err
		}
		if count >= 100 {
			return Target{}, ErrLimit
		}
		result, e := tx.ExecContext(ctx, `INSERT INTO targets(name,url,interval_seconds,timeout_seconds,enabled,created_at,next_check_at,rules) VALUES(?,?,?,?,?,?,?,?)`, in.Name, in.URL, in.IntervalSeconds, in.TimeoutSeconds, in.Enabled, now, now, string(in.Rules))
		if e != nil {
			return Target{}, e
		}
		id, err = result.LastInsertId()
		if err != nil {
			return Target{}, err
		}
	} else {
		result, e := tx.ExecContext(ctx, `UPDATE targets SET name=?,url=?,interval_seconds=?,timeout_seconds=?,enabled=?,next_check_at=?,rules=?,version=version+1 WHERE id=?`, in.Name, in.URL, in.IntervalSeconds, in.TimeoutSeconds, in.Enabled, now, string(in.Rules), id)
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
	indices := map[int64]int{}
	for i, t := range targets {
		indices[t.ID] = i
	}
	cutoff := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	rows, err := s.db.QueryContext(ctx, `SELECT `+checkColumns+` FROM checks WHERE id IN (SELECT MAX(id) FROM checks WHERE checked_at>=? GROUP BY target_id)`, cutoff)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		r, e := scanCheck(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		if idx, ok := indices[r.TargetID]; ok {
			targets[idx].Latest = &r
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT target_id,COUNT(*),COALESCE(SUM(CASE WHEN kind='ok' THEN 1 ELSE 0 END),0),COALESCE(AVG(duration_ms),0) FROM checks WHERE checked_at>=? GROUP BY target_id`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var stats SampleStats
		if err = rows.Scan(&id, &stats.Samples, &stats.Successes, &stats.AverageMS); err != nil {
			return nil, err
		}
		if stats.Samples > 0 {
			stats.Availability = 100 * float64(stats.Successes) / float64(stats.Samples)
		}
		if idx, ok := indices[id]; ok {
			targets[idx].Stats = stats
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
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
	var total any
	if r.TotalDuration > 0 {
		total = float64(r.TotalDuration) / float64(time.Millisecond)
	}
	rules := t.Rules
	if len(rules) == 0 {
		rules = json.RawMessage(`{}`)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at,total_duration_ms,config_version,rules_snapshot) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, t.ID, t.URL, r.URL, r.StatusCode, float64(r.Duration)/float64(time.Millisecond), r.Kind, r.Detail, checkedAt.UnixMilli(), total, t.Version, string(rules))
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
	if err = recordDaily(ctx, tx, t.ID, r, checkedAt, time.Now()); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+checkColumns+` FROM checks WHERE target_id=? AND checked_at>=? AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, id, time.Now().Add(-30*24*time.Hour).UnixMilli(), before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckRecord{}
	for rows.Next() {
		r, e := scanCheck(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func scanCheck(row rowScanner) (CheckRecord, error) {
	var r CheckRecord
	var snapshot sql.NullString
	err := row.Scan(&r.ID, &r.TargetID, &r.InputURL, &r.FinalURL, &r.StatusCode, &r.DurationMS, &r.Kind, &r.Detail, &r.CheckedAt, &r.TotalDurationMS, &r.ConfigVersion, &snapshot)
	if snapshot.Valid {
		r.RulesSnapshot = json.RawMessage(snapshot.String)
	}
	return r, err
}
