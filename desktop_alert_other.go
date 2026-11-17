//go:build !windows

package main

import "errors"

func newDesktopNotifier() (desktopNotifier, error) {
	return nil, errors.New("-desktop-alerts 仅支持 Windows 桌面会话")
}
