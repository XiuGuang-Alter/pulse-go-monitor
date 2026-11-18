package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const schemaV5 = `CREATE TABLE alert_states (
 target_id INTEGER PRIMARY KEY REFERENCES targets(id) ON DELETE CASCADE,
 config_version INTEGER NOT NULL, session TEXT NOT NULL, failures INTEGER NOT NULL CHECK(failures BETWEEN 1 AND 2));
CREATE TABLE alert_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, target_id INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
 target_name TEXT NOT NULL, config_version INTEGER NOT NULL, check_id INTEGER NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('fault','recovery')), error_category TEXT NOT NULL,
 occurred_at INTEGER NOT NULL, notification_status TEXT NOT NULL, notification_reason TEXT NOT NULL DEFAULT '');
CREATE INDEX alert_events_target ON alert_events(target_id,id DESC);
CREATE INDEX alert_events_time ON alert_events(occurred_at);`

// check_id is a historical reference, deliberately not a foreign key: checks
// can be evicted sooner than events under their independent retention limits.
type AlertEvent struct {
	ID                 int64  `json:"id"`
	TargetID           int64  `json:"target_id"`
	TargetName         string `json:"target_name"`
	ConfigVersion      int64  `json:"config_version"`
	CheckID            int64  `json:"check_id"`
	Kind               string `json:"kind"`
	ErrorCategory      string `json:"error_category"`
	OccurredAt         int64  `json:"occurred_at"`
	NotificationStatus string `json:"notification_status"`
	NotificationReason string `json:"notification_reason"`
}

type alertCheckOptions struct {
	session     string
	desktop     bool
	completedAt time.Time
	event       *AlertEvent
}

func (s *Store) recordCheckWithEvents(ctx context.Context, t Target, r Result, checkedAt, completedAt time.Time, session string, desktop bool) (*AlertEvent, error) {
	options := &alertCheckOptions{session: session, desktop: desktop, completedAt: completedAt}
	_, err := s.recordCheck(ctx, t, r, checkedAt, options)
	return options.event, err
}

func recordAlertTx(ctx context.Context, tx *sql.Tx, target Target, result Result, checkID int64, now time.Time, options *alertCheckOptions) (*AlertEvent, error) {
	var version int64
	var session string
	var failures int
	var firstAt, firstCheck, incidentID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT config_version,session,failures,first_failure_at,first_failure_check_id,incident_id
 FROM alert_states WHERE target_id=?`, target.ID).Scan(&version, &session, &failures, &firstAt, &firstCheck, &incidentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if version != target.Version || session != options.session {
		if incidentID.Valid {
			reason := "configuration_changed"
			if session != options.session {
				reason = "restarted"
			}
			if err = interruptTargetIncident(ctx, tx, target.ID, reason, now); err != nil {
				return nil, err
			}
		}
		failures = 0
		incidentID = sql.NullInt64{}
	}
	kind := ""
	at := now.UnixMilli()
	if result.Kind == "ok" {
		if failures == 2 {
			kind = "recovery"
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM alert_states WHERE target_id=?", target.ID)
	} else {
		switch failures {
		case 0:
			firstAt = sql.NullInt64{Int64: at, Valid: true}
			firstCheck = sql.NullInt64{Int64: checkID, Valid: true}
			_, err = tx.ExecContext(ctx, `INSERT INTO alert_states
 (target_id,config_version,session,failures,first_failure_at,first_failure_check_id,incident_id)
 VALUES (?,?,?,1,?,?,NULL) ON CONFLICT(target_id) DO UPDATE SET
 config_version=excluded.config_version,session=excluded.session,failures=1,
 first_failure_at=excluded.first_failure_at,first_failure_check_id=excluded.first_failure_check_id,incident_id=NULL`,
				target.ID, target.Version, options.session, firstAt, firstCheck)
		case 1:
			kind = "fault"
		case 2:
			return nil, recordIncidentFailure(ctx, tx, incidentID.Int64, checkID, at)
		}
	}
	if err != nil || kind == "" {
		return nil, err
	}
	event := &AlertEvent{TargetID: target.ID, TargetName: target.Name, ConfigVersion: target.Version, CheckID: checkID, Kind: kind, ErrorCategory: safeEventCategory(normalizedErrorCategory(result.Kind, result.StatusCode, result.ErrorCategory)), OccurredAt: now.UnixMilli(), NotificationStatus: "disabled"}
	if options.desktop {
		event.NotificationStatus = "queued"
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO alert_events(target_id,target_name,config_version,check_id,kind,error_category,occurred_at,notification_status,notification_reason) VALUES(?,?,?,?,?,?,?,?,?)`, event.TargetID, event.TargetName, event.ConfigVersion, event.CheckID, event.Kind, event.ErrorCategory, event.OccurredAt, event.NotificationStatus, "")
	if err != nil {
		return nil, err
	}
	if event.ID, err = inserted.LastInsertId(); err != nil {
		return nil, err
	}
	if kind == "fault" {
		id, e := createIncident(ctx, tx, options.session, firstAt.Int64, firstCheck.Int64, event)
		if e != nil {
			return nil, e
		}
		if _, err = tx.ExecContext(ctx, "UPDATE alert_states SET failures=2,incident_id=? WHERE target_id=?", id, target.ID); err != nil {
			return nil, err
		}
	} else if err = recoverIncident(ctx, tx, incidentID.Int64, event, now); err != nil {
		return nil, err
	}
	cutoff := now.Add(-30 * 24 * time.Hour).UnixMilli()
	_, err = tx.ExecContext(ctx, `DELETE FROM alert_events WHERE target_id=? AND (occurred_at<? OR id NOT IN (SELECT id FROM alert_events WHERE target_id=? AND occurred_at>=? ORDER BY id DESC LIMIT 1000))`, target.ID, cutoff, target.ID, cutoff)
	return event, err
}

const eventColumns = "id,target_id,target_name,config_version,check_id,kind,error_category,occurred_at,notification_status,notification_reason"

func (s *Store) interruptAlertEvents(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE alert_events SET notification_status='interrupted',notification_reason='' WHERE notification_status IN ('queued','sending')`); err != nil {
		return err
	}
	if err = interruptRuntimeIncidents(ctx, tx, "", "restarted", time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) stopIncidents(ctx context.Context, session string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = interruptRuntimeIncidents(ctx, tx, session, "stopped", time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

// Claim and revalidate in one write statement. Never send an evicted event or
// recreate a row after its target was removed. Configuration can change after
// claiming, but a stale queued event is never intentionally submitted.
func (s *Store) claimAlertEvent(ctx context.Context, event desktopAlert) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	updated, err := tx.ExecContext(ctx, `UPDATE alert_events SET notification_status='sending',notification_reason=''
 WHERE id=? AND target_id=? AND config_version=? AND notification_status='queued' AND occurred_at>=?
 AND EXISTS(SELECT 1 FROM targets WHERE id=? AND enabled=1 AND version=?)`, event.eventID, event.targetID, event.version, time.Now().Add(-30*24*time.Hour).UnixMilli(), event.targetID, event.version)
	if err != nil {
		return false, err
	}
	n, err := updated.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		_, err = tx.ExecContext(ctx, `UPDATE alert_events SET notification_status='skipped',notification_reason='configuration_changed' WHERE id=? AND notification_status='queued'`, event.eventID)
		if err != nil {
			return false, err
		}
	}
	return n == 1, tx.Commit()
}

func (s *Store) finishAlertEvent(ctx context.Context, id int64, status, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alert_events SET notification_status=?,notification_reason=? WHERE id=? AND notification_status IN ('queued','sending')`, status, reason, id)
	return err
}

func safeEventCategory(category string) string {
	switch category {
	case "":
		return ""
	case "dns", "connection", "tls", "timeout", "http_4xx", "http_5xx", "http_other", "body_read", "body_too_large", "assertion", "input", "unknown":
		return category
	default:
		return "unknown"
	}
}

func (s *Store) AlertEvents(ctx context.Context, targetID int64, limit int, before int64) ([]AlertEvent, error) {
	if targetID < 0 || limit < 1 || limit > 200 || before < 0 {
		return nil, fmt.Errorf("%w：target_id与before_id不可为负，limit为1–200", ErrValidation)
	}
	if targetID != 0 {
		if _, err := s.GetTarget(ctx, targetID); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM alert_events WHERE (?=0 OR target_id=?) AND occurred_at>=? AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, targetID, targetID, time.Now().Add(-30*24*time.Hour).UnixMilli(), before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []AlertEvent{}
	for rows.Next() {
		var e AlertEvent
		if err = rows.Scan(&e.ID, &e.TargetID, &e.TargetName, &e.ConfigVersion, &e.CheckID, &e.Kind, &e.ErrorCategory, &e.OccurredAt, &e.NotificationStatus, &e.NotificationReason); err != nil {
			return nil, err
		}
		// Keep output categories fixed even when inspecting a manually changed DB.
		if e.Kind == "recovery" {
			e.ErrorCategory = ""
		} else {
			e.ErrorCategory = safeEventCategory(e.ErrorCategory)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
