//go:build !windows

package main

import (
	"context"
	"errors"
	"io"
)

func withTaskStopEvent(context.Context, string) (context.Context, func(), error) {
	return nil, nil, errors.New("仅 Windows 支持计划任务停止事件")
}

func runPlatformTask(context.Context, string, []string, io.Writer) error {
	return errors.New("仅 Windows 支持计划任务")
}
