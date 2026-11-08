package main

import "time"

const freshnessGrace = 2 * time.Second

type Freshness struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	ObservedAt int64  `json:"observed_at"`
	ValidUntil int64  `json:"valid_until"`
}

func targetFreshness(t Target, now time.Time) Freshness {
	f := Freshness{ObservedAt: now.UnixMilli(), ValidUntil: t.NextCheckAt + int64(t.TimeoutSeconds)*1000 + freshnessGrace.Milliseconds()}
	switch {
	case !t.Enabled:
		f.State = "paused"
	case t.Running:
		f.State = "checking"
		if t.RunningDeadline > 0 {
			f.ValidUntil = t.RunningDeadline
		} else if t.RunningSince > 0 {
			f.ValidUntil = t.RunningSince + int64(t.TimeoutSeconds)*1000 + freshnessGrace.Milliseconds()
		}
		if now.UnixMilli() > f.ValidUntil {
			f.State = "stale"
			f.Reason = "check_overdue"
		}
	case now.UnixMilli() > f.ValidUntil:
		f.State = "stale"
		f.Reason = "schedule_delayed"
	case t.Latest == nil && now.UnixMilli() < t.NextCheckAt:
		f.State = "unsampled"
	case t.Latest == nil:
		f.State = "waiting"
		f.Reason = "queued"
	case t.Latest.ConfigVersion == nil || *t.Latest.ConfigVersion != t.Version:
		f.State = "pending_configuration"
	default:
		f.State = "fresh"
		if now.UnixMilli() >= t.NextCheckAt {
			f.Reason = "queued"
		}
	}
	return f
}
