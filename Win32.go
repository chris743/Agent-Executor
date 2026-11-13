//go:build windows

package main

import (
	"errors"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	dwmapi   = syscall.NewLazyDLL("dwmapi.dll")

	pEnumWindows               = user32.NewProc("EnumWindows")
	pGetWindowTextW            = user32.NewProc("GetWindowTextW")
	pGetWindowTextLengthW      = user32.NewProc("GetWindowTextLengthW")
	pGetClassNameW             = user32.NewProc("GetClassNameW")
	pIsWindowVisible           = user32.NewProc("IsWindowVisible")
	pIsIconic                  = user32.NewProc("IsIconic")
	pGetWindowRect             = user32.NewProc("GetWindowRect")
	pGetWindowThreadProcessId  = user32.NewProc("GetWindowThreadProcessId")
	pGetForegroundWindow       = user32.NewProc("GetForegroundWindow")
	pSetForegroundWindow       = user32.NewProc("SetForegroundWindow")
	pBringWindowToTop          = user32.NewProc("BringWindowToTop")
	pShowWindow                = user32.NewProc("ShowWindow")
	pAttachThreadInput         = user32.NewProc("AttachThreadInput")
	pSendInput                 = user32.NewProc("SendInput")
	pMapVirtualKeyW            = user32.NewProc("MapVirtualKeyW")
	pVkKeyScanW                = user32.NewProc("VkKeyScanW")
	pGetKeyState               = user32.NewProc("GetKeyState")
	pGetCursorPos              = user32.NewProc("GetCursorPos")
	pGetSystemMetrics          = user32.NewProc("GetSystemMetrics")
	pGetDC                     = user32.NewProc("GetDC")
	pReleaseDC                 = user32.NewProc("ReleaseDC")
	pOpenClipboard             = user32.NewProc("OpenClipboard")
	pCloseClipboard            = user32.NewProc("CloseClipboard")
	pEmptyClipboard            = user32.NewProc("EmptyClipboard")
	pGetClipboardData          = user32.NewProc("GetClipboardData")
	pSetClipboardData          = user32.NewProc("SetClipboardData")
	pGetClipboardSeq           = user32.NewProc("GetClipboardSequenceNumber")
	pOpenInputDesktop          = user32.NewProc("OpenInputDesktop")
	pCloseDesktop              = user32.NewProc("CloseDesktop")
	pSetProcessDpiAwarenessCtx = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetProcessDPIAware        = user32.NewProc("SetProcessDPIAware")

	pCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	pCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	pSelectObject           = gdi32.NewProc("SelectObject")
	pBitBlt                 = gdi32.NewProc("BitBlt")
	pCreateDIBSection       = gdi32.NewProc("CreateDIBSection")
	pGdiFlush               = gdi32.NewProc("GdiFlush")
	pDeleteObject           = gdi32.NewProc("DeleteObject")
	pDeleteDC               = gdi32.NewProc("DeleteDC")

	pGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
	pGlobalAlloc        = kernel32.NewProc("GlobalAlloc")
	pGlobalFree         = kernel32.NewProc("GlobalFree")
	pGlobalLock         = kernel32.NewProc("GlobalLock")
	pGlobalUnlock       = kernel32.NewProc("GlobalUnlock")

	pDwmGetWindowAttribute = dwmapi.NewProc("DwmGetWindowAttribute")
)

const (
	cfUnicodeText            = 13
	gmemMoveable             = 0x0002
	swRestore                = 9
	dwmwaExtendedFrameBounds = 9
	desktopSwitchDesktop     = 0x0100
	smXVirtualScreen         = 76
	smYVirtualScreen         = 77
	smCXVirtualScreen        = 78
	smCYVirtualScreen        = 79
	vkCapital                = 0x14
)

type rect struct{ Left, Top, Right, Bottom int32 }
type point struct{ X, Y int32 }

// Rect is a screen-space rectangle in physical pixels.
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func toRect(r rect) Rect {
	return Rect{X: int(r.Left), Y: int(r.Top), W: int(r.Right - r.Left), H: int(r.Bottom - r.Top)}
}

func (r Rect) Contains(x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.W && y < r.Y+r.H
}

func (r Rect) Area() int { return r.W * r.H }

func (r Rect) Union(o Rect) Rect {
	x0, y0 := minInt(r.X, o.X), minInt(r.Y, o.Y)
	x1, y1 := maxInt(r.X+r.W, o.X+o.W), maxInt(r.Y+r.H, o.Y+o.H)
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Window describes a top-level window on the local (client) desktop.
type Window struct {
	HWND      uintptr `json:"hwnd"`
	Title     string  `json:"title"`
	Class     string  `json:"class"`
	PID       uint32  `json:"pid"`
	Rect      Rect    `json:"rect"`
	Minimized bool    `json:"minimized"`
}

var (
	enumMu  sync.Mutex
	enumOut []uintptr
	enumCB  = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		enumOut = append(enumOut, hwnd)
		return 1 // continue enumeration
	})
)

func topLevelWindows() []uintptr {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumOut = enumOut[:0]
	pEnumWindows.Call(enumCB, 0)
	return append([]uintptr(nil), enumOut...)
}

func isVisible(hwnd uintptr) bool {
	r, _, _ := pIsWindowVisible.Call(hwnd)
	return r != 0
}

func isMinimized(hwnd uintptr) bool {
	r, _, _ := pIsIconic.Call(hwnd)
	return r != 0
}

func windowText(hwnd uintptr) string {
	n, _, _ := pGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	pGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return syscall.UTF16ToString(buf)
}

func className(hwnd uintptr) string {
	buf := make([]uint16, 256)
	pGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func windowPID(hwnd uintptr) uint32 {
	if hwnd == 0 {
		return 0
	}
	var pid uint32
	pGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

// windowRect prefers the DWM frame bounds (excludes the invisible resize border/shadow).
func windowRect(hwnd uintptr) Rect {
	var r rect
	if pDwmGetWindowAttribute.Find() == nil {
		hr, _, _ := pDwmGetWindowAttribute.Call(hwnd, dwmwaExtendedFrameBounds,
			uintptr(unsafe.Pointer(&r)), unsafe.Sizeof(r))
		if hr == 0 {
			return toRect(r)
		}
	}
	pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return toRect(r)
}

func describeWindow(hwnd uintptr) Window {
	return Window{
		HWND:      hwnd,
		Title:     windowText(hwnd),
		Class:     className(hwnd),
		PID:       windowPID(hwnd),
		Rect:      windowRect(hwnd),
		Minimized: isMinimized(hwnd),
	}
}

func foregroundWindow() uintptr {
	h, _, _ := pGetForegroundWindow.Call()
	return h
}

// focusWindow brings hwnd to the foreground. Success means the foreground window
// belongs to pid: with RemoteApp, the server may hand focus to one of its dialogs.
func focusWindow(hwnd uintptr, pid uint32) error {
	if windowPID(foregroundWindow()) == pid && !isMinimized(hwnd) {
		return nil
	}
	if isMinimized(hwnd) {
		pShowWindow.Call(hwnd, swRestore)
		time.Sleep(250 * time.Millisecond)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	fg := foregroundWindow()
	cur, _, _ := pGetCurrentThreadId.Call()
	fgThread, _, _ := pGetWindowThreadProcessId.Call(fg, 0)
	if fg != 0 && fgThread != 0 && fgThread != cur {
		// Attaching to the foreground thread's input lets SetForegroundWindow succeed
		// even though this process is a background console app.
		pAttachThreadInput.Call(cur, fgThread, 1)
		defer pAttachThreadInput.Call(cur, fgThread, 0)
	}
	pBringWindowToTop.Call(hwnd)
	pSetForegroundWindow.Call(hwnd)

	for i := 0; i < 20; i++ {
		if windowPID(foregroundWindow()) == pid {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("could not bring the ERP window to the foreground")
}

// inputDesktopAvailable is false when the workstation is locked or a secure
// desktop (UAC prompt, Ctrl+Alt+Del screen) is showing. Input and capture fail then.
func inputDesktopAvailable() bool {
	h, _, _ := pOpenInputDesktop.Call(0, 0, desktopSwitchDesktop)
	if h == 0 {
		return false
	}
	pCloseDesktop.Call(h)
	return true
}

func virtualScreen() Rect {
	x, _, _ := pGetSystemMetrics.Call(smXVirtualScreen)
	y, _, _ := pGetSystemMetrics.Call(smYVirtualScreen)
	w, _, _ := pGetSystemMetrics.Call(smCXVirtualScreen)
	h, _, _ := pGetSystemMetrics.Call(smCYVirtualScreen)
	return Rect{X: int(int32(x)), Y: int(int32(y)), W: int(int32(w)), H: int(int32(h))}
}

func cursorPos() (point, bool) {
	var p point
	r, _, _ := pGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return p, r != 0
}

func capsLockOn() bool {
	r, _, _ := pGetKeyState.Call(vkCapital)
	return r&1 != 0
}

// enableDPIAwareness makes all coordinates physical pixels, so screenshots and
// clicks line up regardless of Windows display scaling.
func enableDPIAwareness() {
	if pSetProcessDpiAwarenessCtx.Find() == nil {
		// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 == (HANDLE)-4
		if r, _, _ := pSetProcessDpiAwarenessCtx.Call(^uintptr(3)); r != 0 {
			return
		}
	}
	if pSetProcessDPIAware.Find() == nil {
		pSetProcessDPIAware.Call()
	}
}

// ---- clipboard ----

func openClipboard() error {
	for i := 0; i < 40; i++ {
		if r, _, _ := pOpenClipboard.Call(0); r != 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("clipboard is busy (another process has it open)")
}

func clipboardSeq() uint32 {
	r, _, _ := pGetClipboardSeq.Call()
	return uint32(r)
}

func readClipboard() (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := openClipboard(); err != nil {
		return "", err
	}
	defer pCloseClipboard.Call()

	h, _, _ := pGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", nil // no text on the clipboard
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		return "", errors.New("GlobalLock on clipboard data failed")
	}
	defer pGlobalUnlock.Call(h)
	return utf16PtrToString(p), nil
}

func writeClipboard(text string) error {
	u, err := syscall.UTF16FromString(text)
	if err != nil {
		return err // text contains a NUL byte
	}
	h, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(len(u)*2))
	if h == 0 {
		return errors.New("GlobalAlloc failed")
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return errors.New("GlobalLock failed")
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(p)), len(u)), u)
	pGlobalUnlock.Call(h)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := openClipboard(); err != nil {
		pGlobalFree.Call(h)
		return err
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	if r, _, _ := pSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		pGlobalFree.Call(h)
		return errors.New("SetClipboardData failed")
	}
	return nil // the system owns h now
}

func utf16PtrToString(p uintptr) string {
	var s []uint16
	for off := uintptr(0); ; off += 2 {
		c := *(*uint16)(unsafe.Pointer(p + off))
		if c == 0 {
			break
		}
		s = append(s, c)
	}
	return string(utf16.Decode(s))
}