package main

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func withTaskStopEvent(parent context.Context, token string) (context.Context, func(), error) {
	owner, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	// Only the current Windows account and SYSTEM can signal this event.
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;" + owner.User.Sid.String() + ")")
	if err != nil {
		return nil, nil, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	name := windows.StringToUTF16Ptr(`Local\PulseStop.` + token)
	handle, err := windows.CreateEvent(&attributes, 1, 0, name)
	if err != nil {
		if handle != 0 {
			windows.CloseHandle(handle)
		}
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return nil, nil, fmt.Errorf("同一计划任务的停止事件已存在：%w", err)
		}
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		for ctx.Err() == nil {
			result, err := windows.WaitForSingleObject(handle, 200)
			if err != nil || result != uint32(windows.WAIT_TIMEOUT) {
				return
			}
		}
	}()
	return ctx, func() {
		cancel()
		<-done
		windows.CloseHandle(handle)
	}, nil
}
