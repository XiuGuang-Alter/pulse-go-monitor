package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"
)

const taskMaxRestarts = 3

func superviseTask(ctx context.Context, delay time.Duration, errOut io.Writer, runWorker func(context.Context) error) error {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil
		}
		err := runWorker(ctx)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		if attempt == taskMaxRestarts {
			return err
		}
		fmt.Fprintf(errOut, "监测进程退出，%s 后重试（%d/%d）：%v\n", delay, attempt+1, taskMaxRestarts, err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func runTaskSupervisor(ctx context.Context, args []string, errOut io.Writer) int {
	flags := flag.NewFlagSet("task-supervise", flag.ContinueOnError)
	flags.SetOutput(errOut)
	addr := flags.String("addr", "127.0.0.1:8080", "本机监听地址")
	db := flags.String("db", "data/monitor.db", "SQLite 数据库路径")
	alerts := flags.Bool("desktop-alerts", false, "Windows 桌面提醒")
	stopToken := flags.String("windows-stop-event", "", "任务脚本设置的停止标识")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || !loopbackAddress(*addr) || *db == "" || !validTaskStopToken(*stopToken) {
		fmt.Fprintln(errOut, "计划任务参数无效，请使用 pulse-task.ps1 安装和管理")
		return 2
	}
	workerArgs := []string{"serve", "-addr", *addr, "-db", *db}
	if *alerts {
		workerArgs = append(workerArgs, "-desktop-alerts")
	}
	if err := runPlatformTask(ctx, *stopToken, workerArgs, errOut); err != nil {
		fmt.Fprintln(errOut, "计划任务异常退出：", err)
		return 1
	}
	return 0
}
