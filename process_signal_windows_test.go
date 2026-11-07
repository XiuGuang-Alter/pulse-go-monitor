package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func isolateConsole(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010, HideWindow: true} // CREATE_NEW_CONSOLE
}
func prepareConsole() error {
	result, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler").Call(0, 0)
	if result == 0 {
		return fmt.Errorf("enable private-console CTRL_C_EVENT: %w", err)
	}
	return nil
}
func interruptChild(_ *os.Process) error {
	result, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent").Call(0, 0)
	if result == 0 {
		return fmt.Errorf("private-console CTRL_C_EVENT: %w", err)
	}
	return nil
}
