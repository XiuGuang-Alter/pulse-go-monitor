package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

type Checker func(context.Context, string, time.Duration) Result

type Scheduler struct {
	store             *Store
	checker           Checker
	ctx               context.Context
	cancel            context.CancelFunc
	slots             chan struct{}
	mu                sync.Mutex
	running           map[int64]bool
	started, stopping bool
	wg                sync.WaitGroup
	pollInterval      time.Duration
	wake              chan struct{}
	retryAt           map[int64]time.Time
	readRetryAt       time.Time
	ready             chan struct{}
}

func NewScheduler(store *Store, concurrency int, checker Checker) (*Scheduler, error) {
	if concurrency < 1 || concurrency > 32 {
		return nil, fmt.Errorf("并发数范围为1–32")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{store: store, checker: checker, ctx: ctx, cancel: cancel, slots: make(chan struct{}, concurrency), running: map[int64]bool{}, pollInterval: 250 * time.Millisecond, wake: make(chan struct{}, 1), retryAt: map[int64]time.Time{}, ready: make(chan struct{})}, nil
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
	targets, err := s.store.DueTargets(s.ctx)
	if err != nil {
		if s.ctx.Err() == nil {
			log.Printf("读取到期目标失败: %v", err)
			s.mu.Lock()
			s.readRetryAt = time.Now().Add(time.Second)
			s.mu.Unlock()
		}
		return
	}
	for _, target := range targets {
		// 再读配置，避免在查到到期目标后，用户恰好暂停或修改它。
		current, err := s.store.GetTarget(s.ctx, target.ID)
		if err != nil {
			if !errors.Is(err, ErrNotFound) && s.ctx.Err() == nil {
				s.mu.Lock()
				s.readRetryAt = time.Now().Add(time.Second)
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
}
func (s *Scheduler) Trigger(id int64) error {
	s.mu.Lock()
	stopping := s.stopping
	s.mu.Unlock()
	if stopping {
		return ErrStopped
	}
	target, err := s.store.GetTarget(s.ctx, id)
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
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { s.mu.Lock(); delete(s.running, target.ID); <-s.slots; s.mu.Unlock(); s.Notify() }()
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
		if err := s.store.RecordCheck(s.ctx, target, result, checkedAt); err != nil && !errors.Is(err, ErrNotFound) && s.ctx.Err() == nil {
			log.Printf("保存目标 %d 结果失败: %v", target.ID, err)
			s.mu.Lock()
			s.retryAt[target.ID] = time.Now().Add(time.Second)
			s.mu.Unlock()
		} else {
			s.mu.Lock()
			delete(s.retryAt, target.ID)
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
