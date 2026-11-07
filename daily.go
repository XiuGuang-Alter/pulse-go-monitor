package main

import (
	"context"
	"database/sql"
	"time"
)

type DailyBucket struct {
	Date             string   `json:"date"`
	Samples          int      `json:"samples"`
	Successes        int      `json:"successes"`
	Availability     *float64 `json:"availability"`
	AverageMS        *float64 `json:"average_ms"`
	MaxMS            *float64 `json:"max_ms"`
	MigrationPartial bool     `json:"migration_partial"`
	InProgress       bool     `json:"in_progress"`
}
type DailyResponse struct {
	Timezone  string        `json:"timezone"`
	StartedAt int64         `json:"started_at"`
	Buckets   []DailyBucket `json:"buckets"`
}

func (s *Store) Daily(ctx context.Context, id int64, days int) (DailyResponse, error) {
	out := DailyResponse{Timezone: "UTC", Buckets: []DailyBucket{}}
	if days < 1 || days > 30 {
		return out, ErrValidation
	}
	if _, err := s.GetTarget(ctx, id); err != nil {
		return out, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM monitor_metadata WHERE key='summary_started_at'").Scan(&out.StartedAt); err != nil {
		return out, err
	}
	today := utcDay(time.Now())
	first := today.AddDate(0, 0, 1-days)
	for i := 0; i < days; i++ {
		out.Buckets = append(out.Buckets, DailyBucket{Date: first.AddDate(0, 0, i).Format("2006-01-02"), InProgress: i == days-1})
	}
	rows, err := s.db.QueryContext(ctx, `SELECT date,samples,successes,duration_sum_ms,max_ms,migration_partial FROM daily_summaries WHERE target_id=? AND date>=? AND date<=? ORDER BY date`, id, first.Format("2006-01-02"), today.Format("2006-01-02"))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	indices := map[string]int{}
	for i, b := range out.Buckets {
		indices[b.Date] = i
	}
	for rows.Next() {
		var date string
		var samples, successes int
		var sum, max float64
		var partial bool
		if err = rows.Scan(&date, &samples, &successes, &sum, &max, &partial); err != nil {
			return out, err
		}
		if samples <= 0 {
			continue
		}
		idx, ok := indices[date]
		if !ok {
			continue
		}
		availability := 100 * float64(successes) / float64(samples)
		average := sum / float64(samples)
		b := &out.Buckets[idx]
		b.Samples = samples
		b.Successes = successes
		b.Availability = &availability
		b.AverageMS = &average
		b.MaxMS = &max
		b.MigrationPartial = partial
	}
	return out, rows.Err()
}
func recordDaily(ctx context.Context, tx *sql.Tx, id int64, r Result, at, now time.Time) error {
	first := utcDay(now).AddDate(0, 0, -29)
	day := utcDay(at)
	if !day.Before(first) && !day.After(utcDay(now)) {
		success := 0
		if r.Kind == "ok" {
			success = 1
		}
		duration := float64(r.Duration) / float64(time.Millisecond)
		if _, err := tx.ExecContext(ctx, `INSERT INTO daily_summaries(target_id,date,samples,successes,duration_sum_ms,max_ms) VALUES(?,?,1,?,?,?)
   ON CONFLICT(target_id,date) DO UPDATE SET samples=samples+1,successes=successes+excluded.successes,duration_sum_ms=duration_sum_ms+excluded.duration_sum_ms,max_ms=MAX(max_ms,excluded.max_ms)`, id, day.Format("2006-01-02"), success, duration, duration); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM daily_summaries WHERE date<?", first.Format("2006-01-02"))
	return err
}
