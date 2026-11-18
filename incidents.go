package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const schemaV6 = `
DELETE FROM alert_states;
ALTER TABLE alert_states ADD COLUMN first_failure_at INTEGER;
ALTER TABLE alert_states ADD COLUMN first_failure_check_id INTEGER;
ALTER TABLE alert_states ADD COLUMN incident_id INTEGER;
CREATE TABLE incidents (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 target_id INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
 target_name TEXT NOT NULL, config_version INTEGER NOT NULL, session TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('active','recovered','interrupted')),
 started_at INTEGER NOT NULL, confirmed_at INTEGER NOT NULL, last_failure_at INTEGER NOT NULL,
 first_failure_check_id INTEGER NOT NULL, last_failure_check_id INTEGER NOT NULL,
 fault_event_id INTEGER NOT NULL, error_category TEXT NOT NULL,
 failure_count INTEGER NOT NULL CHECK(failure_count>=2),
 recovered_at INTEGER, recovery_check_id INTEGER, recovery_event_id INTEGER,
 closed_at INTEGER, interrupt_reason TEXT NOT NULL DEFAULT '', clock_invalid INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX incidents_one_active ON incidents(target_id) WHERE status='active';
CREATE INDEX incidents_target_id ON incidents(target_id,id DESC);
CREATE INDEX incidents_expiry ON incidents(closed_at) WHERE closed_at IS NOT NULL;
CREATE INDEX incidents_closed ON incidents(target_id,closed_at DESC,id DESC) WHERE closed_at IS NOT NULL;`

const incidentRetention = 30 * 24 * time.Hour

type Incident struct {
	ID                  int64  `json:"id"`
	TargetID            int64  `json:"target_id"`
	TargetName          string `json:"target_name"`
	ConfigVersion       int64  `json:"config_version"`
	Status              string `json:"status"`
	StartedAt           int64  `json:"started_at"`
	ConfirmedAt         int64  `json:"confirmed_at"`
	LastFailureAt       int64  `json:"last_failure_at"`
	FirstFailureCheckID int64  `json:"first_failure_check_id"`
	LastFailureCheckID  int64  `json:"last_failure_check_id"`
	FaultEventID        int64  `json:"fault_event_id"`
	ErrorCategory       string `json:"error_category"`
	FailureCount        int64  `json:"failure_count"`
	RecoveredAt         *int64 `json:"recovered_at"`
	RecoveryCheckID     *int64 `json:"recovery_check_id"`
	RecoveryEventID     *int64 `json:"recovery_event_id"`
	ClosedAt            *int64 `json:"closed_at"`
	InterruptReason     string `json:"interrupt_reason"`
	ObservedDurationMS  *int64 `json:"observed_duration_ms"`
	RecoveryDurationMS  *int64 `json:"recovery_duration_ms"`
}

func createIncident(ctx context.Context, tx *sql.Tx, session string, firstAt, firstCheck int64, event *AlertEvent) (int64, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO incidents
 (target_id,target_name,config_version,session,status,started_at,confirmed_at,last_failure_at,
 first_failure_check_id,last_failure_check_id,fault_event_id,error_category,failure_count,clock_invalid)
 VALUES (?,?,?,?,'active',?,?,?,?,?,?,?,2,?)`, event.TargetID, event.TargetName, event.ConfigVersion, session, firstAt,
		event.OccurredAt, event.OccurredAt, firstCheck, event.CheckID, event.ID, event.ErrorCategory, event.OccurredAt < firstAt)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func recordIncidentFailure(ctx context.Context, tx *sql.Tx, id, checkID, at int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE incidents SET failure_count=failure_count+1,
 clock_invalid=clock_invalid OR last_failure_at>?, last_failure_at=?, last_failure_check_id=?
 WHERE id=? AND status='active'`, at, at, checkID, id)
	return err
}

func recoverIncident(ctx context.Context, tx *sql.Tx, id int64, event *AlertEvent, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE incidents SET status='recovered',
 clock_invalid=clock_invalid OR last_failure_at>?, recovered_at=?, closed_at=?, recovery_check_id=?, recovery_event_id=?
 WHERE id=? AND status='active'`, event.OccurredAt, event.OccurredAt, event.OccurredAt, event.CheckID, event.ID, id)
	if err != nil {
		return err
	}
	return pruneIncidents(ctx, tx, event.TargetID, now)
}

func interruptTargetIncident(ctx context.Context, tx *sql.Tx, targetID int64, reason string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE incidents SET status='interrupted', closed_at=?, interrupt_reason=?
 WHERE target_id=? AND status='active'`, now.UnixMilli(), reason, targetID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	return pruneIncidents(ctx, tx, targetID, now)
}

// An empty session selects all records left by earlier processes at startup.
func interruptRuntimeIncidents(ctx context.Context, tx *sql.Tx, session, reason string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE incidents SET status='interrupted', closed_at=?, interrupt_reason=?
 WHERE status='active' AND (?='' OR session=?)`, now.UnixMilli(), reason, session, session)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM alert_states WHERE (?='' OR session=?)", session, session); err != nil {
		return err
	}
	return pruneIncidents(ctx, tx, 0, now)
}

type incidentExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func pruneExpiredIncidents(ctx context.Context, db incidentExecutor, now time.Time) error {
	_, err := db.ExecContext(ctx, "DELETE FROM incidents WHERE closed_at<?", now.Add(-incidentRetention).UnixMilli())
	return err
}

func pruneIncidents(ctx context.Context, db incidentExecutor, targetID int64, now time.Time) error {
	cutoff := now.Add(-incidentRetention).UnixMilli()
	if targetID != 0 {
		_, err := db.ExecContext(ctx, `DELETE FROM incidents WHERE target_id=? AND closed_at IS NOT NULL
 AND (closed_at<? OR id IN (SELECT id FROM incidents WHERE target_id=? AND closed_at>=?
 ORDER BY closed_at DESC,id DESC LIMIT -1 OFFSET 1000))`, targetID, cutoff, targetID, cutoff)
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM incidents WHERE closed_at IS NOT NULL AND
 (closed_at<? OR id IN (SELECT id FROM (
 SELECT id, row_number() OVER(PARTITION BY target_id ORDER BY closed_at DESC,id DESC) AS rank
 FROM incidents WHERE closed_at>=?) WHERE rank>1000))`, cutoff, cutoff)
	return err
}

func (s *Store) Incidents(ctx context.Context, targetID int64, status string, limit int, before int64, observedAt time.Time) ([]Incident, error) {
	if targetID < 0 || limit < 1 || limit > 200 || before < 0 {
		return nil, fmt.Errorf("%w：target_id和before_id不可为负，limit为1–200", ErrValidation)
	}
	switch status {
	case "", "active", "recovered", "interrupted":
	default:
		return nil, fmt.Errorf("%w：未知故障状态", ErrValidation)
	}
	if targetID != 0 {
		if _, err := s.GetTarget(ctx, targetID); err != nil {
			return nil, err
		}
	}
	query := `SELECT id,target_id,target_name,config_version,status,started_at,confirmed_at,last_failure_at,
 first_failure_check_id,last_failure_check_id,fault_event_id,error_category,failure_count,
 recovered_at,recovery_check_id,recovery_event_id,closed_at,interrupt_reason,clock_invalid
 FROM incidents WHERE (closed_at IS NULL OR closed_at>=?)`
	args := []any{observedAt.Add(-incidentRetention).UnixMilli()}
	if targetID != 0 {
		query += " AND target_id=?"
		args = append(args, targetID)
	}
	if status != "" {
		query += " AND status=?"
		args = append(args, status)
	}
	if before != 0 {
		query += " AND id<?"
		args = append(args, before)
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Incident{}
	for rows.Next() {
		var item Incident
		var invalidClock bool
		err = rows.Scan(&item.ID, &item.TargetID, &item.TargetName, &item.ConfigVersion, &item.Status,
			&item.StartedAt, &item.ConfirmedAt, &item.LastFailureAt, &item.FirstFailureCheckID, &item.LastFailureCheckID,
			&item.FaultEventID, &item.ErrorCategory, &item.FailureCount, &item.RecoveredAt, &item.RecoveryCheckID,
			&item.RecoveryEventID, &item.ClosedAt, &item.InterruptReason, &invalidClock)
		if err != nil {
			return nil, err
		}
		item.ErrorCategory = safeEventCategory(item.ErrorCategory)
		if !invalidClock && item.StartedAt >= 0 && item.ConfirmedAt >= item.StartedAt && item.LastFailureAt >= item.ConfirmedAt {
			span := item.LastFailureAt - item.StartedAt
			item.ObservedDurationMS = &span
			if item.RecoveredAt != nil && *item.RecoveredAt >= item.LastFailureAt {
				duration := *item.RecoveredAt - item.StartedAt
				item.RecoveryDurationMS = &duration
			}
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
