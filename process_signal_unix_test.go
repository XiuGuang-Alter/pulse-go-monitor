//go:build !windows

package main

import (
	"os"
	"os/exec"
)

func isolateConsole(_ *exec.Cmd)               {}
func prepareConsole() error                    { return nil }
func interruptChild(process *os.Process) error { return process.Signal(os.Interrupt) }
