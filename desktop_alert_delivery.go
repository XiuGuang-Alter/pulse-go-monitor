package main

import (
	"context"
	"errors"
	"log"
	"runtime"
	"sync/atomic"
	"time"
)

const desktopAlertQueueCapacity = 100

// The delivery goroutine owns the notifier for its entire lifecycle.
type desktopNotifier interface {
	send(context.Context, desktopAlert) (shown bool, err error)
	pump()
	close()
}

type desktopAlertDelivery struct {
	ctx     context.Context
	cancel  context.CancelFunc
	queue   chan desktopAlert
	done    chan struct{}
	dropped atomic.Uint64
	logger  *log.Logger
}

func startDesktopAlertDelivery(parent context.Context, store *Store, notifier desktopNotifier, logger *log.Logger) *desktopAlertDelivery {
	ctx, cancel := context.WithCancel(parent)
	delivery := &desktopAlertDelivery{
		ctx: ctx, cancel: cancel, queue: make(chan desktopAlert, desktopAlertQueueCapacity), done: make(chan struct{}), logger: logger,
	}
	go delivery.run(store, notifier)
	return delivery
}

func (d *desktopAlertDelivery) enqueue(event desktopAlert) {
	if d.ctx.Err() != nil {
		return
	}
	select {
	case d.queue <- event:
	default:
		d.dropped.Add(1)
	}
}

func (d *desktopAlertDelivery) run(store *Store, notifier desktopNotifier) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(d.done)
	defer notifier.close()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for d.ctx.Err() == nil {
		select {
		case <-d.ctx.Done():
			return
		case <-tick.C:
			notifier.pump()
			if n := d.dropped.Swap(0); n != 0 {
				d.logger.Printf("桌面提醒队列已满，跳过 %d 条提醒", n)
			}
		case event := <-d.queue:
			notifier.pump()
			d.deliver(store, notifier, event)
		}
	}
}

func (d *desktopAlertDelivery) deliver(store *Store, notifier desktopNotifier, event desktopAlert) {
	ctx, cancel := context.WithTimeout(d.ctx, storeOperationTimeout)
	target, err := store.GetTarget(ctx, event.targetID)
	cancel()
	if d.ctx.Err() != nil || errors.Is(err, ErrNotFound) {
		return
	}
	if err != nil {
		d.logger.Printf("目标 %d 桌面提醒提交失败（无法确认当前配置）", event.targetID)
		return
	}
	if !target.Enabled || target.Version != event.version {
		return
	}
	shown, err := notifier.send(d.ctx, event)
	if err != nil {
		if d.ctx.Err() == nil {
			d.logger.Printf("目标 %d 桌面提醒提交失败（Windows 通知区域不可用）", event.targetID)
		}
		return
	}
	state := "故障"
	if event.recovered {
		state = "恢复"
	}
	status := "未收到 Windows 显示回调"
	if shown {
		status = "Windows 已报告显示"
	}
	d.logger.Printf("目标 %d 桌面提醒已提交（%s），%s", event.targetID, state, status)
}

func (d *desktopAlertDelivery) stop() {
	d.cancel()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	select {
	case <-d.done:
	case <-timeout.C:
		d.logger.Print("等待 Windows 桌面提醒清理超时")
	}
}
