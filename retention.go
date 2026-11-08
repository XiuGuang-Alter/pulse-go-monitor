package main

import (
	"context"
	"time"
)

type RetentionInfo struct {
	MaxRecords         int    `json:"max_records"`
	MaxAgeDays         int    `json:"max_age_days"`
	TheoreticalSeconds int64  `json:"theoretical_seconds"`
	Records            int    `json:"records"`
	EarliestCheckedAt  *int64 `json:"earliest_checked_at"`
	LatestCheckedAt    *int64 `json:"latest_checked_at"`
}

func (s *Store) Retention(ctx context.Context, id int64) (RetentionInfo, error) {
	target, err := s.GetTarget(ctx, id)
	if err != nil {
		return RetentionInfo{}, err
	}
	r := RetentionInfo{MaxRecords: 1000, MaxAgeDays: 30, TheoreticalSeconds: int64(target.IntervalSeconds) * 1000}
	if r.TheoreticalSeconds > 30*86400 {
		r.TheoreticalSeconds = 30 * 86400
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*),MIN(checked_at),MAX(checked_at) FROM checks WHERE target_id=? AND checked_at>=?`, id, time.Now().Add(-30*24*time.Hour).UnixMilli()).Scan(&r.Records, &r.EarliestCheckedAt, &r.LatestCheckedAt)
	return r, err
}
