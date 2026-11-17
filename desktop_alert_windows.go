package main

import (
	"context"
	"errors"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nimSetVersion = 4
	nifMessage    = 0x01
	nifIcon       = 0x02
	nifTip        = 0x04
	nifInfo       = 0x10
	nifShowTip    = 0x80
	niifInfo      = 0x01
	niifWarning   = 0x02
	niifQuietTime = 0x80

	desktopIconMessage = 0x8001 // WM_APP + 1
	balloonShow        = 0x0402 // NIN_BALLOONSHOW
	balloonHide        = 0x0403
	balloonTimeout     = 0x0404
	balloonClick       = 0x0405
)

var (
	shellNotifyIcon = windows.NewLazySystemDLL("shell32.dll").NewProc("Shell_NotifyIconW")
	userDLL         = windows.NewLazySystemDLL("user32.dll")
	createWindow    = userDLL.NewProc("CreateWindowExW")
	destroyWindow   = userDLL.NewProc("DestroyWindow")
	loadIcon        = userDLL.NewProc("LoadIconW")
	peekMessage     = userDLL.NewProc("PeekMessageW")
	translateMsg    = userDLL.NewProc("TranslateMessage")
	dispatchMessage = userDLL.NewProc("DispatchMessageW")
	controlsDLL     = windows.NewLazySystemDLL("comctl32.dll")
	setSubclass     = controlsDLL.NewProc("SetWindowSubclass")
	removeSubclass  = controlsDLL.NewProc("RemoveWindowSubclass")
	defSubclass     = controlsDLL.NewProc("DefSubclassProc")
	alertOwners     sync.Map // HWND -> *shellDesktopNotifier
	alertWindowProc = windows.NewCallback(handleDesktopIconMessage)
)

// notifyIconData matches NOTIFYICONDATAW from shellapi.h.
type notifyIconData struct {
	size                uint32
	window              uintptr
	id, flags, callback uint32
	icon                uintptr
	tip                 [128]uint16
	state, stateMask    uint32
	info                [256]uint16
	version             uint32
	title               [64]uint16
	infoFlags           uint32
	guid                windows.GUID
	balloonIcon         uintptr
}

// winMessage holds a Win32 MSG while the owning thread pumps messages.
type winMessage struct {
	window         uintptr
	message        uint32
	wParam, lParam uintptr
	time           uint32
	x, y           int32
	private        uint32
}

type shellDesktopNotifier struct {
	window uintptr
	icon   uintptr
	id     uint16
	added  bool
	wait   bool
	shown  bool
}

func newDesktopNotifier() (desktopNotifier, error) { return &shellDesktopNotifier{id: 1}, nil }

func (n *shellDesktopNotifier) data() notifyIconData {
	return notifyIconData{size: uint32(unsafe.Sizeof(notifyIconData{})), window: n.window, id: uint32(n.id)}
}

func (n *shellDesktopNotifier) open() error {
	if n.window != 0 {
		return nil
	}
	var module windows.Handle
	if err := windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, nil, &module); err != nil {
		return err
	}
	class := windows.StringToUTF16Ptr("STATIC")
	name := windows.StringToUTF16Ptr("Pulse")
	const wsExToolWindow = 0x80
	window, _, _ := createWindow.Call(wsExToolWindow, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(name)), 0, 0, 0, 0, 0, 0, 0, uintptr(module), 0)
	if window == 0 {
		return errors.New("cannot create Windows notification owner")
	}
	const idiApplication = 32512
	icon, _, _ := loadIcon.Call(0, idiApplication)
	if icon == 0 {
		destroyWindow.Call(window)
		return errors.New("cannot load Windows notification icon")
	}
	if handled, _, _ := setSubclass.Call(window, alertWindowProc, 1, 0); handled == 0 {
		destroyWindow.Call(window)
		return errors.New("cannot receive Windows notification events")
	}
	n.window, n.icon = window, icon
	alertOwners.Store(window, n)
	return nil
}

func handleDesktopIconMessage(window, message, wParam, lParam, _, _ uintptr) uintptr {
	if message == desktopIconMessage {
		if owner, ok := alertOwners.Load(window); ok {
			notifier := owner.(*shellDesktopNotifier)
			if notifier.wait && uint16(lParam>>16) == notifier.id {
				switch uint16(lParam) {
				case balloonShow:
					notifier.shown = true
				case balloonHide, balloonTimeout, balloonClick:
					notifier.wait = false
				}
			}
		}
		return 0
	}
	result, _, _ := defSubclass.Call(window, message, wParam, lParam)
	return result
}

func (n *shellDesktopNotifier) addIcon() bool {
	data := n.data()
	data.flags, data.icon, data.callback = nifIcon|nifTip|nifShowTip|nifMessage, n.icon, desktopIconMessage
	copy(data.tip[:], utf16.Encode([]rune("Pulse")))
	added, _, _ := shellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&data)))
	n.added = added != 0
	if n.added {
		data.version = 4 // NOTIFYICON_VERSION_4
		if ok, _, _ := shellNotifyIcon.Call(nimSetVersion, uintptr(unsafe.Pointer(&data))); ok == 0 {
			n.removeIcon()
		}
	}
	return n.added
}

func (n *shellDesktopNotifier) send(ctx context.Context, event desktopAlert) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := n.open(); err != nil {
		return false, err
	}
	if !n.added && !n.addIcon() {
		return false, errors.New("cannot register Windows notification icon")
	}
	data := n.data()
	data.flags = nifInfo
	data.infoFlags = niifQuietTime | niifWarning
	title := "Pulse · 故障"
	if event.recovered {
		title = "Pulse · 恢复"
		data.infoFlags = niifQuietTime | niifInfo
	}
	copy(data.title[:], utf16.Encode([]rune(title)))
	name := make([]rune, 0, len(event.name))
	for _, r := range event.name {
		if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
			name = append(name, r)
		}
	}
	text := utf16.Encode(name)
	if len(text) == 0 {
		text = utf16.Encode([]rune("监测目标"))
	}
	// Keep the fixed Windows buffer terminated and retain complete runes.
	if len(text) >= len(data.info) {
		text = text[:len(data.info)-1]
		if last := text[len(text)-1]; last >= 0xd800 && last <= 0xdbff {
			text = text[:len(text)-1]
		}
	}
	copy(data.info[:], text)
	n.wait, n.shown = true, false
	sent, _, _ := shellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&data)))
	if sent == 0 && n.addIcon() { // Explorer may have restarted since the last alert.
		sent, _, _ = shellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&data)))
	}
	if sent == 0 {
		n.wait = false
		return false, errors.New("Windows rejected notification")
	}
	return n.waitForDisplay(ctx)
}

// Keep one banner in flight per icon. Windows reports when it is shown and when
// it closes or is suppressed. A withdrawn/expired banner gets a new icon ID so
// a late callback cannot be mistaken for the next alert.
func (n *shellDesktopNotifier) waitForDisplay(ctx context.Context) (bool, error) {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for n.wait {
		n.pump()
		if !n.wait {
			break
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			n.wait = false
			n.removeIcon()
			n.id++
			if n.id == 0 {
				n.id = 1
			}
		case <-tick.C:
		}
	}
	return n.shown, nil
}

func (n *shellDesktopNotifier) pump() {
	if n.window == 0 {
		return
	}
	const pmRemove = 1
	var message winMessage
	for range 32 {
		found, _, _ := peekMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0, pmRemove)
		if found == 0 {
			return
		}
		translateMsg.Call(uintptr(unsafe.Pointer(&message)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
	}
}

func (n *shellDesktopNotifier) removeIcon() {
	if n.added {
		data := n.data()
		shellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&data)))
		n.added = false
	}
}

func (n *shellDesktopNotifier) close() {
	if n.window == 0 {
		return
	}
	n.wait = false
	n.removeIcon()
	alertOwners.Delete(n.window)
	removeSubclass.Call(n.window, alertWindowProc, 1)
	destroyWindow.Call(n.window)
	n.window, n.icon = 0, 0
}
