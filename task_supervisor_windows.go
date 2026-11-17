package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func runPlatformTask(parent context.Context, token string, workerArgs []string, errOut io.Writer) error {
	ctx, cleanup, err := withTaskStopEvent(parent, token)
	if err != nil {
		return err
	}
	defer cleanup()
	path, err := os.Executable()
	if err != nil {
		return err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(job)
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return err
	}
	return superviseTask(ctx, time.Minute, errOut, func(ctx context.Context) error {
		return runTaskWorker(ctx, job, path, workerArgs, errOut)
	})
}

func runTaskWorker(ctx context.Context, job windows.Handle, path string, args []string, errOut io.Writer) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(random[:])
	workerArgs := append(append([]string(nil), args...), "-windows-stop-event", token)
	cmd := exec.Command(path, workerArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := attachTaskWorker(job, cmd.Process.Pid); err != nil {
		// Never leave a process running after protection against orphaning failed.
		_ = cmd.Process.Kill()
		if waitErr := cmd.Wait(); waitErr != nil {
			return fmt.Errorf("无法将监测进程加入 Windows 作业：%w", err)
		}
		return nil // the short-lived worker had already exited normally
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return stopTaskWorker(token, done, errOut)
	}
}

func attachTaskWorker(job windows.Handle, pid int) error {
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(proc)
	return windows.AssignProcessToJobObject(job, proc)
}

func stopTaskWorker(token string, done <-chan error, errOut io.Writer) error {
	name := windows.StringToUTF16Ptr(`Local\PulseStop.` + token)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		handle, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
		if err == nil {
			err = windows.SetEvent(handle)
			windows.CloseHandle(handle)
			if err == nil {
				return <-done
			}
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			fmt.Fprintln(errOut, "无法向监测进程发送正常停止信号，继续等待进程退出：", err)
			return <-done
		}
		select {
		case err := <-done:
			return err
		case <-tick.C:
		}
	}
}
