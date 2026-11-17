package main

import "time"

type desktopAlert struct {
	targetID  int64
	version   int64
	name      string
	recovered bool
}

type desktopAlertState struct {
	version  int64
	failures uint8
	updated  time.Time
}

// The scheduler mutex protects the tracker. Two failures open an incident.
// An entry is needed only while there is a failure to remember.
type desktopAlertTracker map[int64]desktopAlertState

func (tracker *desktopAlertTracker) observe(target Target, ok bool) (desktopAlert, bool) {
	state := (*tracker)[target.ID]
	if state.version != target.Version {
		state = desktopAlertState{version: target.Version}
	}
	event := desktopAlert{targetID: target.ID, version: target.Version, name: target.Name, recovered: ok}
	if ok {
		delete(*tracker, target.ID)
		return event, state.failures == 2
	}
	if *tracker == nil {
		*tracker = make(desktopAlertTracker)
	}
	state.updated = time.Now()
	if state.failures < 2 {
		state.failures++
		(*tracker)[target.ID] = state
		return event, state.failures == 2
	}
	(*tracker)[target.ID] = state
	return event, false
}

func (tracker desktopAlertTracker) prune(live map[int64]bool, scanStarted time.Time) {
	for id, state := range tracker {
		if !live[id] && state.updated.Before(scanStarted) {
			delete(tracker, id)
		}
	}
}
