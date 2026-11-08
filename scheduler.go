package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

type Checker func(context.Context, string, time.Duration) Result

type Scheduler struct {
	runningSince            map[int64]int64
	runningDeadlines        map[int64]int64
	store                   *Store
	checker                 Checker
	ctx                     context.Context
	cancel                  context.CancelFunc
	slots                   chan struct{}
	mu                      sync.Mutex
	running                 map[int64]bool
	started, stopping       bool
	wg                      sync.WaitGroup
	pollInterval            time.Duration
	wake                    chan struct{}
	retryAt                 map[int64]time.Time
	readRetryAt             time.Time
	ready                   chan struct{}
	createdAt               time.Time
	instanceID              string
	lastScanAt, lastWriteAt int64
	successfulWrites        uint64
	readFailures            int
	writeFailures           map[int64]int
	enabledIDs              map[int64]bool
}

func NewScheduler(store *Store, concurrency int, checker Checker) (*Scheduler, error) {
	if concurrency < 1 || concurrency > 32 {
		return nil, fmt.Errorf("并发数范围为1–32")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{runningSince: map[int64]int64{}, runningDeadlines: map[int64]int64{}, createdAt: time.Now(), instanceID: fmt.Sprintf("%x", rand.Text()), writeFailures: map[int64]int{}, enabledIDs: map[int64]bool{}, store: store, checker: checker, ctx: ctx, cancel: cancel, slots: make(chan struct{}, concurrency), running: map[int64]bool{}, pollInterval: 250 * time.Millisecond, wake: make(chan struct{}, 1), retryAt: map[int64]time.Time{}, ready: make(chan struct{})}, nil
}
func (s *Scheduler) Notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopping {
		return
	}
	s.started = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var ticks <-chan time.Time
		if s.pollInterval > 0 {
			ticker := time.NewTicker(s.pollInterval)
			defer ticker.Stop()
			ticks = ticker.C
		}
		first := true
		for {
			s.scheduleDue()
			if first {
				close(s.ready)
				first = false
			}
			select {
			case <-s.ctx.Done():
				return
			case <-ticks:
			case <-s.wake:
			}
		}
	}()
}
func (s *Scheduler) scheduleDue() {
	s.mu.Lock()
	for id, at := range s.retryAt {
		if !time.Now().Before(at) {
			delete(s.retryAt, id)
		}
	}
	retry := s.readRetryAt
	stopped := s.stopping
	s.mu.Unlock()
	if stopped || time.Now().Before(retry) {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, storeOperationTimeout)
	defer cancel()
	targets, err := s.store.targets(ctx, "ORDER BY next_check_at,id")
	if err != nil {
		if s.ctx.Err() == nil {
			log.Printf("读取到期目标失败（%T）", err)
			s.mu.Lock()
			s.readRetryAt = time.Now().Add(time.Second)
			s.readFailures++
			s.mu.Unlock()
		}
		return
	}
	s.mu.Lock()
	live := map[int64]bool{}
	s.enabledIDs = map[int64]bool{}
	for _, t := range targets {
		live[t.ID] = true
		if t.Enabled {
			s.enabledIDs[t.ID] = true
		}
	}
	for id := range s.writeFailures {
		if !live[id] {
			delete(s.writeFailures, id)
		}
	}
	s.mu.Unlock()
	for _, target := range targets {
		if !target.Enabled || target.NextCheckAt > time.Now().UnixMilli() {
			continue
		}
		// 再读配置，避免在查到到期目标后，用户恰好暂停或修改它。
		current, err := s.store.GetTarget(ctx, target.ID)
		if err != nil {
			if !errors.Is(err, ErrNotFound) && s.ctx.Err() == nil {
				s.mu.Lock()
				s.readRetryAt = time.Now().Add(time.Second)
				s.readFailures++
				s.mu.Unlock()
				return
			}
			continue
		}
		if err != nil || !current.Enabled || current.NextCheckAt > time.Now().UnixMilli() {
			continue
		}
		s.launch(current)
	}
	s.mu.Lock()
	s.lastScanAt = time.Now().UnixMilli()
	s.readFailures = 0
	s.mu.Unlock()
}
func (s *Scheduler) Trigger(id int64) error {
	s.mu.Lock()
	stopping := s.stopping
	s.mu.Unlock()
	if stopping {
		return ErrStopped
	}
	ctx, cancel := context.WithTimeout(s.ctx, storeOperationTimeout)
	defer cancel()
	target, err := s.store.GetTarget(ctx, id)
	if err != nil {
		return err
	}
	return s.launch(target)
}
func (s *Scheduler) launch(target Target) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return ErrStopped
	}
	if s.running[target.ID] {
		return ErrBusy
	}
	if time.Now().Before(s.retryAt[target.ID]) {
		return ErrBusy
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return ErrBusy
	}
	s.running[target.ID] = true
	s.runningSince[target.ID] = time.Now().UnixMilli()
	s.runningDeadlines[target.ID] = s.runningSince[target.ID] + int64(target.TimeoutSeconds)*1000 + freshnessGrace.Milliseconds()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.running, target.ID)
			delete(s.runningSince, target.ID)
			delete(s.runningDeadlines, target.ID)
			<-s.slots
			s.mu.Unlock()
			s.Notify()
		}()
		checkedAt := time.Now()
		var result Result
		if s.checker != nil {
			result = s.checker(s.ctx, target.URL, time.Duration(target.TimeoutSeconds)*time.Second)
		} else {
			rules, err := parseRules(target.Rules)
			if err != nil {
				result = Result{URL: target.URL, Kind: "invalid_input", Detail: "目标规则无效"}
			} else {
				result = CheckWithRules(s.ctx, target.URL, time.Duration(target.TimeoutSeconds)*time.Second, rules)
			}
		}
		if s.ctx.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, storeOperationTimeout)
		defer cancel()
		err := s.store.RecordCheck(ctx, target, result, checkedAt)
		if err != nil && !errors.Is(err, ErrNotFound) && s.ctx.Err() == nil {
			log.Printf("保存目标 %d 结果失败（%T）", target.ID, err)
			s.mu.Lock()
			s.retryAt[target.ID] = time.Now().Add(time.Second)
			s.writeFailures[target.ID]++
			s.mu.Unlock()
		} else {
			s.mu.Lock()
			delete(s.retryAt, target.ID)
			delete(s.writeFailures, target.ID)
			if err == nil {
				s.successfulWrites++
				s.lastWriteAt = time.Now().UnixMilli()
			}
			s.mu.Unlock()
		}
	}()
	return nil
}
func (s *Scheduler) RunningIDs() map[int64]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]bool, len(s.running))
	for id := range s.running {
		out[id] = true
	}
	return out
}
func (s *Scheduler) Stop() { s.mu.Lock(); s.stopping = true; s.cancel(); s.mu.Unlock(); s.wg.Wait() }

type RunningSnapshot struct{ StartedAt, Deadline int64 }

func (s *Scheduler) RunningTimes() map[int64]RunningSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]RunningSnapshot, len(s.runningSince))
	for id, at := range s.runningSince {
		out[id] = RunningSnapshot{StartedAt: at, Deadline: s.runningDeadlines[id]}
	}
	return out
}
