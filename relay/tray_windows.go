//go:build windows

package main

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

//go:embed relay.ico
var iconData []byte

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	pRegisterClassEx  = user32.NewProc("RegisterClassExW")
	pCreateWindowEx   = user32.NewProc("CreateWindowExW")
	pDefWindowProc    = user32.NewProc("DefWindowProcW")
	pGetMessage       = user32.NewProc("GetMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessage  = user32.NewProc("DispatchMessageW")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pPostMessage      = user32.NewProc("PostMessageW")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	pAppendMenu       = user32.NewProc("AppendMenuW")
	pTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	pDestroyMenu      = user32.NewProc("DestroyMenu")
	pGetCursorPos     = user32.NewProc("GetCursorPos")
	pSetForeground    = user32.NewProc("SetForegroundWindow")
	pLoadImage        = user32.NewProc("LoadImageW")
	pRegisterWinMsg   = user32.NewProc("RegisterWindowMessageW")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	pMessageBox       = user32.NewProc("MessageBoxW")
	pShellNotifyIcon  = shell32.NewProc("Shell_NotifyIconW")
	pShellExecute     = shell32.NewProc("ShellExecuteW")
	pGetModuleHandle  = kernel32.NewProc("GetModuleHandleW")
	pRegOpenKeyEx     = advapi32.NewProc("RegOpenKeyExW")
	pRegSetValueEx    = advapi32.NewProc("RegSetValueExW")
	pRegDeleteValue   = advapi32.NewProc("RegDeleteValueW")
	pRegQueryValueEx  = advapi32.NewProc("RegQueryValueExW")
	pIsDialogMessage  = user32.NewProc("IsDialogMessageW")
	pShowWindow       = user32.NewProc("ShowWindow")
	pSetFocus         = user32.NewProc("SetFocus")
	pSendMessage      = user32.NewProc("SendMessageW")
	pGetWindowText    = user32.NewProc("GetWindowTextW")
	pLoadCursor       = user32.NewProc("LoadCursorW")
	pGetStockObject   = gdi32.NewProc("GetStockObject")
	pRegCloseKey      = advapi32.NewProc("RegCloseKey")
)

const (
	wmClose      = 0x0010
	wmDestroy    = 0x0002
	wmUser       = 0x0400
	wmTray       = wmUser + 1
	wmNull       = 0x0000
	wmLButtonDbl = 0x0203
	wmRButtonUp  = 0x0205

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2
	nifMsg    = 0x1
	nifIcon   = 0x2
	nifTip    = 0x4
	nifInfo   = 0x10

	mfString    = 0x0
	mfGrayed    = 0x1
	mfDisabled  = 0x2
	mfChecked   = 0x8
	mfSeparator = 0x800

	tpmRightButton = 0x2
	tpmReturnCmd   = 0x100

	idOpen   = 1
	idLog    = 2
	idAuto   = 3
	idExit   = 4
	idPort   = 5
	idDir    = 6
	idOK     = 1
	idCancel = 2
)

type point struct{ X, Y int32 }

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type notifyIconData struct {
	Size            uint32
	Hwnd            uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GuidItem        [16]byte
	BalloonIcon     uintptr
}

func utf16p(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func copyU16(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}

func dataDir() string {
	d := filepath.Join(os.Getenv("LOCALAPPDATA"), "SimpleVoiceOBS")
	os.MkdirAll(d, 0o755)
	return d
}

func logPath() string { return filepath.Join(dataDir(), "relay.log") }

func initLog() {
	p := logPath()
	if st, err := os.Stat(p); err == nil && st.Size() > 1<<20 {
		os.Remove(p)
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		logOut = f
	}
}

func messageBox(text string, flags uintptr) {
	pMessageBox.Call(0, uintptr(unsafe.Pointer(utf16p(text))), uintptr(unsafe.Pointer(utf16p("SimpleVoice OBS Relay"))), flags)
}

func fatal(text string) {
	logf("%s", text)
	messageBox(text, 0x10) // MB_ICONERROR
}

func shellOpen(verbTarget, params string) {
	var pp uintptr
	if params != "" {
		pp = uintptr(unsafe.Pointer(utf16p(params)))
	}
	pShellExecute.Call(0, uintptr(unsafe.Pointer(utf16p("open"))), uintptr(unsafe.Pointer(utf16p(verbTarget))), pp, 0, 1)
}

// openPath shows a folder in Explorer.
func openPath(p string) error {
	r, _, _ := pShellExecute.Call(0, uintptr(unsafe.Pointer(utf16p("open"))), uintptr(unsafe.Pointer(utf16p(p))), 0, 0, 1)
	if r <= 32 {
		return syscall.EINVAL
	}
	return nil
}

// ---- start with Windows (HKCU\...\Run) ----

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runName = "SimpleVoiceOBS"

func openRunKey() (uintptr, bool) {
	var h uintptr
	r, _, _ := pRegOpenKeyEx.Call(0x80000001, uintptr(unsafe.Pointer(utf16p(runKey))), 0, 0x3, uintptr(unsafe.Pointer(&h)))
	return h, r == 0
}

func autostartOn() bool {
	h, ok := openRunKey()
	if !ok {
		return false
	}
	defer pRegCloseKey.Call(h)
	r, _, _ := pRegQueryValueEx.Call(h, uintptr(unsafe.Pointer(utf16p(runName))), 0, 0, 0, 0)
	return r == 0
}

func setAutostart(on bool) {
	h, ok := openRunKey()
	if !ok {
		return
	}
	defer pRegCloseKey.Call(h)
	if !on {
		pRegDeleteValue.Call(h, uintptr(unsafe.Pointer(utf16p(runName))))
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	val, _ := syscall.UTF16FromString(`"` + exe + `"`)
	pRegSetValueEx.Call(h, uintptr(unsafe.Pointer(utf16p(runName))), 0, 1, uintptr(unsafe.Pointer(&val[0])), uintptr(len(val)*2))
}

// ---- "Change port" window ----

var portDlg uintptr
var portEdit uintptr
var applyPort func(string) bool
var currentPort string

func openPortDialog() {
	if portDlg != 0 {
		pSetForeground.Call(portDlg)
		return
	}
	inst, _, _ := pGetModuleHandle.Call(0)
	class := utf16p("SimpleVoiceOBSPortWindow")
	sw, _, _ := pGetSystemMetrics.Call(0)
	sh, _, _ := pGetSystemMetrics.Call(1)
	w, h := uintptr(330), uintptr(165)
	const style = 0x80000000 | 0x00C00000 | 0x00080000 // popup, caption, system menu
	dlg, _, _ := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(utf16p("Change port"))), style,
		(sw-w)/2, (sh-h)/2, w, h, 0, 0, inst, 0)
	if dlg == 0 {
		return
	}
	portDlg = dlg
	font, _, _ := pGetStockObject.Call(17) // DEFAULT_GUI_FONT
	child := func(cls, text string, style uintptr, x, y, cw, ch, id uintptr) uintptr {
		c, _, _ := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(utf16p(cls))), uintptr(unsafe.Pointer(utf16p(text))), 0x40000000|0x10000000|style,
			x, y, cw, ch, dlg, id, inst, 0)
		pSendMessage.Call(c, 0x30, font, 1) // WM_SETFONT
		return c
	}
	child("STATIC", "Port (1024 - 65535):", 0, 16, 16, 280, 18, 0)
	portEdit = child("EDIT", currentPort, 0x00800000|0x00010000|0x2000|0x80, 16, 40, 120, 24, 10) // border, tabstop, number, autohscroll
	child("BUTTON", "OK", 0x00010000|0x1, 130, 85, 80, 26, idOK)
	child("BUTTON", "Cancel", 0x00010000, 220, 85, 80, 26, idCancel)
	pSendMessage.Call(portEdit, 0xB1, 0, ^uintptr(0)) // EM_SETSEL all
	pShowWindow.Call(dlg, 5)
	pSetForeground.Call(dlg)
	pSetFocus.Call(portEdit)
}

func registerPortClass(inst uintptr) {
	cb := syscall.NewCallback(func(h, m, wp, lp uintptr) uintptr {
		switch uint32(m) {
		case 0x0111: // WM_COMMAND
			switch wp & 0xFFFF {
			case idOK:
				buf := make([]uint16, 16)
				pGetWindowText.Call(portEdit, uintptr(unsafe.Pointer(&buf[0])), 16)
				if applyPort(syscall.UTF16ToString(buf)) {
					pDestroyWindow.Call(h)
				}
			case idCancel:
				pDestroyWindow.Call(h)
			}
			return 0
		case wmClose:
			pDestroyWindow.Call(h)
			return 0
		case wmDestroy:
			portDlg = 0
			return 0
		}
		ret, _, _ := pDefWindowProc.Call(h, m, wp, lp)
		return ret
	})
	cur, _, _ := pLoadCursor.Call(0, 32512) // IDC_ARROW
	wc := wndClassEx{WndProc: cb, Instance: inst, Cursor: cur, Background: 16, ClassName: utf16p("SimpleVoiceOBSPortWindow")}
	wc.Size = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
}

// ---- tray ----

func runUI(r *relay, cfg config) {
	runtime.LockOSThread()

	// the icon file is read by Windows from disk, so write it out once
	iconFile := filepath.Join(os.TempDir(), "simplevoice-obs.ico")
	os.WriteFile(iconFile, iconData, 0o644)
	small, _, _ := pGetSystemMetrics.Call(49) // SM_CXSMICON
	hIcon, _, _ := pLoadImage.Call(0, uintptr(unsafe.Pointer(utf16p(iconFile))), 1, small, small, 0x10)

	currentPort = cfg.port
	tip := "SimpleVoice OBS Relay - port " + cfg.port
	taskbarCreated, _, _ := pRegisterWinMsg.Call(uintptr(unsafe.Pointer(utf16p("TaskbarCreated"))))

	var hwnd uintptr
	nid := func() *notifyIconData {
		n := &notifyIconData{Hwnd: hwnd, ID: 1, Flags: nifMsg | nifIcon | nifTip, CallbackMessage: wmTray, Icon: hIcon}
		n.Size = uint32(unsafe.Sizeof(*n))
		copyU16(n.Tip[:], tip)
		return n
	}
	notify := func(op uintptr, n *notifyIconData) { pShellNotifyIcon.Call(op, uintptr(unsafe.Pointer(n))) }
	balloon := func(title, text string) {
		n := nid()
		n.Flags = nifInfo
		copyU16(n.InfoTitle[:], title)
		copyU16(n.Info[:], text)
		n.InfoFlags = 1 // info icon
		notify(nimModify, n)
	}

	showMenu := func() {
		m, _, _ := pCreatePopupMenu.Call()
		defer pDestroyMenu.Call(m)
		add := func(flags, id uintptr, text string) {
			var p uintptr
			if text != "" {
				p = uintptr(unsafe.Pointer(utf16p(text)))
			}
			pAppendMenu.Call(m, flags, id, p)
		}
		add(mfString|mfGrayed|mfDisabled, 100, "SimpleVoice OBS Relay "+version+" - engine: "+r.eng.status().State)
		add(mfString, idOpen, "Open control panel (voices, test)")
		add(mfString, idDir, "Open voices folder")
		add(mfString, idLog, "Show log")
		add(mfString, idPort, "Change port...")
		autoFlags := uintptr(mfString)
		if autostartOn() {
			autoFlags |= mfChecked
		}
		add(autoFlags, idAuto, "Start with Windows")
		add(mfSeparator, 0, "")
		add(mfString|mfGrayed|mfDisabled, 101, "Free and open source (GPL-3.0)")
		add(mfString, idExit, "Stop relay and exit")
		var pt point
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		pSetForeground.Call(hwnd) // required so the menu closes when you click elsewhere
		cmd, _, _ := pTrackPopupMenu.Call(m, tpmRightButton|tpmReturnCmd, uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
		pPostMessage.Call(hwnd, wmNull, 0, 0)
		switch cmd {
		case idOpen:
			shellOpen("http://127.0.0.1:"+cfg.port+"/", "")
		case idLog:
			shellOpen("notepad.exe", `"`+logPath()+`"`)
		case idPort:
			openPortDialog()
		case idDir:
			openPath(r.voices.dir)
		case idAuto:
			setAutostart(!autostartOn())
		case idExit:
			pPostMessage.Call(hwnd, wmClose, 0, 0)
		}
	}

	wndProc := syscall.NewCallback(func(h, m, wp, lp uintptr) uintptr {
		switch uint32(m) {
		case wmTray:
			switch uint32(lp) {
			case wmRButtonUp:
				showMenu()
			case wmLButtonDbl:
				shellOpen("http://127.0.0.1:"+cfg.port+"/", "")
			}
			return 0
		case wmClose:
			notify(nimDelete, nid())
			r.requestQuit()
			pDestroyWindow.Call(h)
			return 0
		case wmDestroy:
			pPostQuitMessage.Call(0)
			return 0
		}
		if taskbarCreated != 0 && uint32(m) == uint32(taskbarCreated) {
			notify(nimAdd, nid()) // Explorer restarted: put the icon back
			return 0
		}
		ret, _, _ := pDefWindowProc.Call(h, m, wp, lp)
		return ret
	})

	inst, _, _ := pGetModuleHandle.Call(0)
	registerPortClass(inst)
	applyPort = func(v string) bool {
		v = strings.TrimSpace(v)
		if !validPort(v) {
			messageBox("Please enter a number from 1024 to 65535.", 0x30)
			return false
		}
		if v == cfg.port {
			return true
		}
		if err := writeSetting("PORT", v); err != nil {
			messageBox("Could not save the new port: "+err.Error(), 0x10)
			return false
		}
		messageBox("Port changed to "+v+". The relay restarts now.\n\nThen:\n1. In the extension settings, set \"Relay address\" to http://127.0.0.1:"+v+" and Save.\n2. In OBS, replace the Browser Source URL with the new one (Copy in the extension settings).", 0x40)
		if exe, err := os.Executable(); err == nil {
			exec.Command(exe, "--restarted").Start()
		}
		pPostMessage.Call(hwnd, wmClose, 0, 0)
		return true
	}
	class := utf16p("SimpleVoiceOBSWindow")
	wc := wndClassEx{WndProc: wndProc, Instance: inst, ClassName: class}
	wc.Size = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	hwnd, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(utf16p("SimpleVoice OBS Relay"))), 0, 0, 0, 0, 0, 0, 0, inst, 0)
	if hwnd == 0 {
		fatal("Could not create the notification-area icon.")
		return
	}
	notify(nimAdd, nid())
	balloon("SimpleVoice OBS Relay", "Running. Double-click this icon to add voices and test.")

	// closing from outside (--stop) ends the message loop through the window
	go func() {
		<-r.quit
		pPostMessage.Call(hwnd, wmClose, 0, 0)
	}()

	var m msg
	for {
		ret, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		if portDlg != 0 {
			if handled, _, _ := pIsDialogMessage.Call(portDlg, uintptr(unsafe.Pointer(&m))); handled != 0 {
				continue
			}
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}
