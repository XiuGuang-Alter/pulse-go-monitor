package main

import "time"

const storeOperationTimeout = 5 * time.Second
const scanHealthGrace = 15 * time.Second

type MonitorHealth struct {
	State             string `json:"state"`
	Reason            string `json:"reason"`
	InstanceID        string `json:"instance_id"`
	ObservedAt        int64  `json:"observed_at"`
	LastScanAt        int64  `json:"last_scan_at"`
	LastWriteAt       int64  `json:"last_write_at"`
	SuccessfulWrites  uint64 `json:"successful_writes"`
	ConsecutiveErrors int    `json:"consecutive_errors"`
	AffectedTargets   int    `json:"affected_targets"`
}

// Health reads only bounded in-memory state; SQL never holds this mutex.
func (s *Scheduler) Health(now time.Time) MonitorHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := MonitorHealth{State: "healthy", InstanceID: s.instanceID, ObservedAt: now.UnixMilli(), LastScanAt: s.lastScanAt, LastWriteAt: s.lastWriteAt, SuccessfulWrites: s.successfulWrites, ConsecutiveErrors: s.readFailures}
	for id, n := range s.writeFailures {
		if s.enabledIDs[id] || s.running[id] {
			h.AffectedTargets++
			h.ConsecutiveErrors += n
		}
	}
	switch {
	case s.stopping:
		h.State = "degraded"
		h.Reason = "stopped"
	case s.readFailures > 0:
		h.State = "degraded"
		h.Reason = "storage_read_failed"
	case now.Sub(s.createdAt) > scanHealthGrace && (s.lastScanAt == 0 || now.UnixMilli()-s.lastScanAt > scanHealthGrace.Milliseconds()):
		h.State = "degraded"
		h.Reason = "scan_not_advancing"
	case h.AffectedTargets > 0:
		h.State = "degraded"
		h.Reason = "storage_write_failed"
	case s.lastScanAt == 0:
		h.State = "starting"
	case len(s.enabledIDs) == 0 && len(s.running) == 0:
		h.State = "idle"
	}
	return h
}
