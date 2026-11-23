// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"errors"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// RegisterHotKey on a thread of its own, locked, with its own message loop (ADR 0034 §2): the key's
// WM_HOTKEY goes to the queue of the thread that registered it. Windows says when the key goes down and not
// when it goes up, so a timer polls it with GetAsyncKeyState while it is down. M25 ran it from a task shaped
// as Norite's, conhost --headless at least privilege, with Notepad in front: a tap and holds of 1.5 s and
// 3 s came back as 0.090, 1.501 and 3.002 s, and Notepad received nothing. From a service's window station
// it fails, which a logon task never is.

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	registerHotKey   = user32.NewProc("RegisterHotKey")
	unregisterHotKey = user32.NewProc("UnregisterHotKey")
	getMessage       = user32.NewProc("GetMessageW")
	postThreadMsg    = user32.NewProc("PostThreadMessageW")
	setTimer         = user32.NewProc("SetTimer")
	killTimer        = user32.NewProc("KillTimer")
	getAsyncKeyState = user32.NewProc("GetAsyncKeyState")
)

const (
	wmQuit   = 0x0012
	wmTimer  = 0x0113
	wmHotkey = 0x0312
	// hotkeyID is the one key a binding registers.
	hotkeyID = 1
	// pollEvery is how often a key down is asked whether it is still down: 10 ms, half a frame.
	pollEvery = 10
	// errAlreadyRegistered is ERROR_HOTKEY_ALREADY_REGISTERED, another program holding the key.
	errAlreadyRegistered = 1409
)

// msg is winuser.h's MSG.
type msg struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       struct{ x, y int32 }
	lPrivate uint32
}

// Default is this OS's keys.
func Default() Binder { return win{} }

type win struct{}

func (win) Bind(name string, ev Events) func() {
	thread := make(chan uint32, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		thread <- windows.GetCurrentThreadId()
		bindWindows(name, ev)
	}()
	return func() {
		// A WM_QUIT posted before the thread has a queue is lost; GetMessage made one before any key was
		// registered, and PostThreadMessage fails on a thread with none, so it is posted until it lands.
		tid := <-thread
		for {
			if r, _, _ := postThreadMsg.Call(uintptr(tid), wmQuit, 0, 0); r != 0 {
				break
			}
			select {
			case <-done:
				return
			default:
				runtime.Gosched()
			}
		}
		<-done
	}
}

func bindWindows(name string, ev Events) {
	key, err := ParseKey(name)
	if err != nil {
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: err.Error()})
		return
	}
	mods, vk := windowsKey(key)
	r, _, callErr := registerHotKey.Call(0, hotkeyID, uintptr(mods), uintptr(vk))
	if r == 0 {
		why := "Windows refused the key"
		var errno syscall.Errno
		if errors.As(callErr, &errno) && errno == errAlreadyRegistered {
			why = "another program holds the key"
		}
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: why})
		// The loop still runs, so the WM_QUIT that ends the binding has somewhere to land.
		drain()
		return
	}
	defer func() { _, _, _ = unregisterHotKey.Call(0, hotkeyID) }()
	ev.Status(Status{State: ipc.VoiceHotkeyRegistered})

	var m msg
	var timer uintptr
	down := false
	for {
		r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // WM_QUIT, or a failure, either of which ends the loop
			if timer != 0 {
				_, _, _ = killTimer.Call(0, timer)
			}
			return
		}
		switch {
		case m.message == wmHotkey && m.wParam == hotkeyID && !down:
			down = true
			ev.Held(true)
			timer, _, _ = setTimer.Call(0, 0, pollEvery, 0)
		case m.message == wmTimer && down && m.wParam == timer:
			if state, _, _ := getAsyncKeyState.Call(uintptr(vk)); state&0x8000 == 0 {
				down = false
				_, _, _ = killTimer.Call(0, timer)
				timer = 0
				ev.Held(false)
			}
		}
	}
}

// drain runs the thread's message loop until its WM_QUIT.
func drain() {
	var m msg
	for {
		if r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
			return
		}
	}
}
