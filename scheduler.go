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
}

func NewScheduler(store *Store, concurrency int, checker Checker) (*Scheduler, error) {
	if concurrency < 1 || concurrency > 32 {
		return nil, fmt.Errorf("并发数范围为1–32")
	}
	if checker == nil {
		checker = Check
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{store: store, checker: checker, ctx: ctx, cancel: cancel, slots: make(chan struct{}, concurrency), running: map[int64]bool{}}, nil
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
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			s.scheduleDue()
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (s *Scheduler) scheduleDue() {
	targets, err := s.store.DueTargets(s.ctx)
	if err != nil {
		if s.ctx.Err() == nil {
			log.Printf("读取到期目标失败: %v", err)
		}
		return
	}
	for _, target := range targets {
		// 再读配置，避免在查到到期目标后，用户恰好暂停或修改它。
		current, err := s.store.GetTarget(s.ctx, target.ID)
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
	select {
	case s.slots <- struct{}{}:
	default:
		return ErrBusy
	}
	s.running[target.ID] = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { s.mu.Lock(); delete(s.running, target.ID); <-s.slots; s.mu.Unlock() }()
		checkedAt := time.Now()
		result := s.checker(s.ctx, target.URL, time.Duration(target.TimeoutSeconds)*time.Second)
		if s.ctx.Err() != nil {
			return
		}
		if err := s.store.RecordCheck(s.ctx, target, result, checkedAt); err != nil && !errors.Is(err, ErrNotFound) && s.ctx.Err() == nil {
			log.Printf("保存目标 %d 结果失败: %v", target.ID, err)
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
