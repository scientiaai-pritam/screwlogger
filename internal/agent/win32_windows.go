//go:build windows

package agent

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                      = windows.NewLazySystemDLL("user32.dll")
	procGetForegroundWindow     = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessI = user32.NewProc("GetWindowThreadProcessId")
	procGetLastInputInfo        = user32.NewProc("GetLastInputInfo")
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount            = kernel32.NewProc("GetTickCount")
)

// lastInputInfo mirrors winuser.h LASTINPUTINFO.
type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// win32Sources implements ForegroundSource and IdleSource with real Win32 calls.
type win32Sources struct{}

// NewWin32Sources returns production poll sources (Windows only).
func NewWin32Sources() (ForegroundSource, IdleSource) { return win32Sources{}, win32Sources{} }

// ForegroundApp returns the executable base name of the foreground window,
// or "unknown.exe" when it cannot be resolved (locked screen, elevated window).
func (win32Sources) ForegroundApp() string {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return "unknown.exe"
	}
	var pid uint32
	procGetWindowThreadProcessI.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return "unknown.exe"
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "unknown.exe"
	}
	defer windows.CloseHandle(h)
	var buf [syscall.MAX_PATH]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "unknown.exe"
	}
	return baseName(windows.UTF16ToString(buf[:size]))
}

// IdleSeconds returns seconds since last input via GetLastInputInfo (spec §3.1).
func (win32Sources) IdleSeconds() float64 {
	var li lastInputInfo
	li.cbSize = uint32(unsafe.Sizeof(li))
	if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&li))); r == 0 {
		return 0
	}
	now, _, _ := procGetTickCount.Call()
	return IdleFromTickCount(uint32(now), li.dwTime)
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '\\' || p[i] == '/' {
			return p[i+1:]
		}
	}
	if p == "" {
		return "unknown.exe"
	}
	return p
}

// compile-time interface checks
var (
	_ ForegroundSource = win32Sources{}
	_ IdleSource       = win32Sources{}
)
