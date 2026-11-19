package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"
)

// An incident check contains only the fields used in the detail view.
type incidentCheck struct {
	ID            int64  `json:"id"`
	CheckedAt     int64  `json:"checked_at"`
	Kind          string `json:"kind"`
	StatusCode    int    `json:"status_code"`
	ErrorCategory string `json:"error_category"`
	Detail        string `json:"detail"`
}

type incidentDetail struct {
	ID            int64           `json:"id"`
	TargetName    string          `json:"target_name"`
	ConfigVersion int64           `json:"config_version"`
	Status        string          `json:"status"`
	StartedAt     int64           `json:"started_at"`
	ConfirmedAt   int64           `json:"confirmed_at"`
	LastFailureAt int64           `json:"last_failure_at"`
	RecoveredAt   *int64          `json:"recovered_at"`
	ClosedAt      *int64          `json:"closed_at"`
	Total         int64           `json:"total"`
	Retained      int             `json:"retained"`
	Cleared       int64           `json:"cleared"`
	Items         []incidentCheck `json:"items"`
}

func (s *Store) incidentDetail(ctx context.Context, id int64, now time.Time) (incidentDetail, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return incidentDetail{}, err
	}
	defer tx.Rollback()
	var detail incidentDetail
	var targetID, first, last int64
	var recovery *int64
	err = tx.QueryRowContext(ctx, `SELECT id,target_name,config_version,status,started_at,confirmed_at,
		last_failure_at,recovered_at,closed_at,failure_count,target_id,first_failure_check_id,last_failure_check_id,recovery_check_id
		FROM incidents WHERE id=? AND (closed_at IS NULL OR closed_at>=?)`, id, now.Add(-incidentRetention).UnixMilli()).Scan(
		&detail.ID, &detail.TargetName, &detail.ConfigVersion, &detail.Status, &detail.StartedAt, &detail.ConfirmedAt,
		&detail.LastFailureAt, &detail.RecoveredAt, &detail.ClosedAt, &detail.Total, &targetID, &first, &last, &recovery)
	if err != nil {
		return incidentDetail{}, err
	}
	if detail.Status == "recovered" {
		detail.Total++
	} else {
		recovery = nil
	}

	// IDs bracket consecutive failures for this target configuration even if the
	// clock moves backwards. Recovery, when observed, has its own exact check ID.
	rows, err := tx.QueryContext(ctx, `SELECT id,checked_at,kind,status_code,coalesce(error_category,''),detail
		FROM checks WHERE target_id=? AND config_version=? AND checked_at>=?
		AND ((id BETWEEN ? AND ? AND kind<>'ok') OR (id=? AND kind='ok')) ORDER BY id`,
		targetID, detail.ConfigVersion, now.Add(-30*24*time.Hour).UnixMilli(), first, last, recovery)
	if err != nil {
		return incidentDetail{}, err
	}
	defer rows.Close()
	detail.Items = []incidentCheck{}
	for rows.Next() {
		var item incidentCheck
		if err = rows.Scan(&item.ID, &item.CheckedAt, &item.Kind, &item.StatusCode, &item.ErrorCategory, &item.Detail); err != nil {
			return incidentDetail{}, err
		}
		item.ErrorCategory = safeEventCategory(normalizedErrorCategory(item.Kind, item.StatusCode, item.ErrorCategory))
		item.Detail = safeDetail(item.Kind, item.StatusCode, item.Detail)
		detail.Items = append(detail.Items, item)
	}
	if err = rows.Err(); err != nil {
		return incidentDetail{}, err
	}
	detail.Retained = len(detail.Items)
	detail.Cleared = detail.Total - int64(detail.Retained)
	return detail, tx.Commit()
}

func (a *App) incidentChecks(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		apiError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storeOperationTimeout)
	defer cancel()
	detail, err := a.store.incidentDetail(ctx, id, time.Now())
	if errors.Is(err, sql.ErrNoRows) {
		jsonResponse(w, http.StatusNotFound, map[string]string{"error": "故障记录已不存在"})
		return
	}
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, detail)
}
