package main

import (
	"context"
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
	store   *Store
}

func startDesktopAlertDelivery(parent context.Context, store *Store, notifier desktopNotifier, logger *log.Logger) *desktopAlertDelivery {
	ctx, cancel := context.WithCancel(parent)
	delivery := &desktopAlertDelivery{
		ctx: ctx, cancel: cancel, queue: make(chan desktopAlert, desktopAlertQueueCapacity), done: make(chan struct{}), logger: logger, store: store,
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
		d.finish(event, "queue_full", "")
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
	claimed, err := store.claimAlertEvent(ctx, event)
	cancel()
	if d.ctx.Err() != nil {
		return
	}
	if err != nil {
		d.logger.Printf("目标 %d 桌面提醒提交失败（无法确认当前配置）", event.targetID)
		d.finish(event, "failed", "configuration_unavailable")
		return
	}
	if !claimed {
		return
	}
	shown, err := notifier.send(d.ctx, event)
	if err != nil {
		if d.ctx.Err() == nil {
			d.logger.Printf("目标 %d 桌面提醒提交失败（Windows 通知区域不可用）", event.targetID)
			d.finish(event, "failed", "notifier_unavailable")
		}
		return
	}
	state := "故障"
	if event.recovered {
		state = "恢复"
	}
	status := "未收到 Windows 显示回调"
	deliveryStatus := "submitted_unconfirmed"
	if shown {
		status = "Windows 已报告显示"
		deliveryStatus = "shown"
	}
	d.finish(event, deliveryStatus, "")
	d.logger.Printf("目标 %d 桌面提醒已提交（%s），%s", event.targetID, state, status)
}

func (d *desktopAlertDelivery) finish(event desktopAlert, status, reason string) {
	if event.eventID == 0 || d.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, storeOperationTimeout)
	defer cancel()
	if err := d.store.finishAlertEvent(ctx, event.eventID, status, reason); err != nil && d.ctx.Err() == nil {
		d.logger.Printf("提醒 %d 状态保存失败，重启后将标记中断", event.eventID)
	}
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
