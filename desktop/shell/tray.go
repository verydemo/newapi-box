//go:build windows

// 托盘与窗口消息处理。
//
// 关掉窗口默认不是退出：拦下 WM_CLOSE，把窗口藏起来，程序继续在后台跑，
// 右下角留一个托盘图标。真正退出走托盘右键菜单。
package main

import (
	"runtime/debug"
	"syscall"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
)

const (
	wmClose         = 0x0010
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmApp           = 0x8000
	trayCallbackMsg = wmApp + 1

	swHide = 0
	swShow = 5

	// SetWindowLongPtrW 的 GWLP_WNDPROC（-4 的 32 位补码）
	gwlpWndProc = 0xFFFFFFFC

	nimAdd     = 0x00000000
	nimDelete  = 0x00000002
	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	mfString       = 0x00000000
	mfSeparator    = 0x00000800
	tpmReturnCmd   = 0x00000100
	tpmRightButton = 0x00000002

	idiApplication = 32512

	cmdShowWindow = 1
	cmdExit       = 2

	iconResourceID = 1 // versioninfo.json 里嵌入的图标资源 ID
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procSetWindowLongPtrW   = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProcW     = user32.NewProc("CallWindowProcW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procLoadIconW           = user32.NewProc("LoadIconW")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

// notifyIconData 是 NOTIFYICONDATAW 的 Go 版本，字段顺序与对齐跟 C 一致。
type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

type point struct{ x, y int32 }

// tray 持有窗口句柄与原始窗口过程，负责图标注册与恢复。
type tray struct {
	hwnd     uintptr
	view     webview2.WebView
	previous uintptr
	data     notifyIconData
	// suspended 表示窗口藏进托盘时已把页面切到 about:blank，
	// 释放控制台占用的渲染内存；恢复窗口时再重新加载。
	suspended bool
}

// newTray 接管窗口消息并挂上托盘图标。
func newTray(hwnd uintptr, view webview2.WebView) *tray {
	t := &tray{hwnd: hwnd, view: view}

	callback := syscall.NewCallback(t.wndProc)
	previous, _, _ := procSetWindowLongPtrW.Call(hwnd, gwlpWndProc, callback)
	t.previous = previous

	t.data = notifyIconData{
		hWnd:             hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: trayCallbackMsg,
		hIcon:            t.loadIcon(),
	}
	t.data.cbSize = uint32(unsafe.Sizeof(t.data))
	copyTip(&t.data, "newapi box · 双击显示窗口，右键更多")
	procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&t.data)))
	return t
}

// loadIcon 优先用 exe 里嵌的图标，取不到就退回系统默认图标。
func (t *tray) loadIcon() uintptr {
	instance, _, _ := procGetModuleHandleW.Call(0)
	if icon, _, _ := procLoadIconW.Call(instance, iconResourceID); icon != 0 {
		return icon
	}
	icon, _, _ := procLoadIconW.Call(0, idiApplication)
	return icon
}

func (t *tray) remove() {
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&t.data)))
}

func (t *tray) restore() {
	if t.suspended {
		// 控制台是纯静态页加接口取数，重新加载即可拿到最新数据，
		// 比常驻一整套 DOM/JS 状态省内存。
		t.view.Navigate(consoleURL)
		t.suspended = false
	}
	procShowWindow.Call(t.hwnd, swShow)
	procSetForegroundWindow.Call(t.hwnd)
}

func (t *tray) hide() {
	if !t.suspended && t.view != nil {
		// 藏进托盘就不再需要控制台页面：切到空白页让渲染进程释放
		// DOM/JS/图片的内存，等价于一个轻量的"挂起"。
		t.view.Navigate("about:blank")
		t.suspended = true
		// Go 侧顺势把堆归还给操作系统；延迟一下避开还活着的请求。
		go func() {
			time.Sleep(2 * time.Second)
			debug.FreeOSMemory()
		}()
	}
	procShowWindow.Call(t.hwnd, swHide)
}

func (t *tray) visible() bool {
	ok, _, _ := procIsWindowVisible.Call(t.hwnd)
	return ok != 0
}

// wndProc 接管窗口消息：WM_CLOSE 变成隐藏，托盘图标负责把它叫回来。
func (t *tray) wndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmClose:
		t.hide()
		return 0
	case trayCallbackMsg:
		switch lparam {
		case wmLButtonUp, wmLButtonDblClk:
			t.restore()
		case wmRButtonUp:
			t.showMenu()
		}
		return 0
	}
	previous, _, _ := procCallWindowProcW.Call(t.previous, hwnd, msg, wparam, lparam)
	return previous
}

// showMenu 在光标处弹出托盘菜单，选中项直接返回，无需再处理 WM_COMMAND。
func (t *tray) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	appendItem(menu, mfString, cmdShowWindow, "显示窗口")
	appendItem(menu, mfSeparator, 0, "")
	appendItem(menu, mfString, cmdExit, "退出")

	var cursor point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	// 弹菜单前必须先抢到前台，否则点空白处菜单不会消失
	procSetForegroundWindow.Call(t.hwnd)

	choice, _, _ := procTrackPopupMenu.Call(menu,
		tpmReturnCmd|tpmRightButton,
		uintptr(cursor.x), uintptr(cursor.y), 0, t.hwnd, 0)

	switch choice {
	case cmdShowWindow:
		t.restore()
	case cmdExit:
		t.remove()
		t.view.Terminate() // 让 Run() 返回，进程随之结束
	}
}

func appendItem(menu uintptr, flags uintptr, id uintptr, label string) {
	var text uintptr
	if label != "" {
		wide, err := syscall.UTF16PtrFromString(label)
		if err != nil {
			return
		}
		text = uintptr(unsafe.Pointer(wide))
	}
	procAppendMenuW.Call(menu, flags, id, text)
}

func copyTip(data *notifyIconData, tip string) {
	wide, err := syscall.UTF16FromString(tip)
	if err != nil {
		return
	}
	limit := len(data.szTip) - 1
	if len(wide) > limit {
		wide = wide[:limit]
	}
	copy(data.szTip[:], wide)
}
