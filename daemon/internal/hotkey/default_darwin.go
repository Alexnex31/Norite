// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// Carbon's RegisterEventHotKey, through purego with no cgo, which needs no permission at all (ADR 0034 §2):
// M25 ran it from a LaunchAgent with Accessibility and Input Monitoring both ungranted, and a tap and a 2 s
// hold came back as one press and one release each. Carbon dispatches its events only through an
// application run loop on the process's main thread, so the daemon's main gives that thread to
// ServeMainThread, which runs NSApplication in "prohibited" activation mode (no Dock icon, no menu bar) once a
// key is first wanted, and never before.
//
// Written against Apple's documentation and M25's probe, and first run on the author's Mac at M35's pass
// (M23's rule for a platform nobody ran).

const frameworks = "/System/Library/Frameworks/"

// Carbon's constants (CarbonEvents.h, Events.h).
const (
	kEventClassKeyboard      = 0x6b657962 // 'keyb'
	kEventHotKeyPressed      = 5
	kEventHotKeyReleased     = 6
	kEventParamDirectObject  = 0x2d2d2d2d // '----'
	typeEventHotKeyID        = 0x686b6964 // 'hkid'
	eventHotKeyExistsErr     = -9878
	hotKeySignature          = 0x4e525445 // 'NRTE'
	nsApplicationProhibited  = 2
	appStartWithin           = 5 * time.Second
	mainThreadCallWithin     = 5 * time.Second
	errNoApplicationLoopText = "this program runs no application loop on its main thread"
)

var (
	// served is set once the daemon's main thread is ServeMainThread's, wanted closed when a key is first
	// asked for, and ready closed once the application runs, appErr saying why it could not.
	served   atomic.Bool
	wanted   = make(chan struct{})
	wantOnce sync.Once
	ready    = make(chan struct{})
	appErr   error

	// What runs on the main thread: dispatch_async_f onto the main queue, with work, one callback for every
	// call, taking the function queued under its context.
	dispatchAsyncF func(queue, ctx, fn uintptr)
	mainQueue      uintptr
	work           uintptr
	queuedMu       sync.Mutex
	queued         = map[uintptr]func(){}
	nextQueued     uintptr

	// Carbon's, resolved once the application runs.
	appTarget  uintptr
	register   func(code, mods uint32, id uint64, target uintptr, opts uint32, out *uintptr) int32
	unregister func(ref uintptr) int32

	// keys routes a hot key's events to its binding, by the id it was registered under.
	keysMu sync.Mutex
	keys   = map[uint32]chan<- bool{}
	nextID uint32
)

// ServeMainThread is the daemon's main thread from its start, locked to it by the daemon's main: it starts
// run on a goroutine and waits for it to end, exiting with its code, and once a key is first wanted runs the
// application loop Carbon needs until then. It never returns. It is marked as served before run starts, so
// run's Default always finds it.
func ServeMainThread(run func() int) {
	served.Store(true)
	done := make(chan int, 1)
	go func() { done <- run() }()
	select {
	case code := <-done:
		os.Exit(code)
	case <-wanted:
	}
	app, err := startApplication()
	appErr = err
	close(ready)
	if err != nil {
		os.Exit(<-done)
	}
	go func() { os.Exit(<-done) }()
	app.Send(objc.RegisterName("run"))
	select {} // run never returns; nothing here runs after it
}

// Default is this OS's keys.
func Default() Binder {
	if !served.Load() {
		return Unavailable(errNoApplicationLoopText)
	}
	return mac{}
}

type mac struct{}

func (mac) Bind(name string, ev Events) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		bindMac(name, ev, stop)
	}()
	return func() {
		close(stop)
		<-done
	}
}

func bindMac(name string, ev Events, stop <-chan struct{}) {
	unavailable := func(why string) { ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: why}) }
	key, err := ParseKey(name)
	if err != nil {
		unavailable(err.Error())
		return
	}
	mods, code, err := macKey(key)
	if err != nil {
		unavailable(err.Error())
		return
	}
	wantOnce.Do(func() { close(wanted) })
	select {
	case <-ready:
	case <-stop:
		return
	case <-time.After(appStartWithin):
		unavailable("the application loop the key needs did not start")
		return
	}
	if appErr != nil {
		unavailable(appErr.Error())
		return
	}
	events := make(chan bool, 16)
	keysMu.Lock()
	nextID++
	id := nextID
	keys[id] = events
	keysMu.Unlock()
	defer func() {
		keysMu.Lock()
		delete(keys, id)
		keysMu.Unlock()
	}()
	var ref uintptr
	var rc int32
	ran, ok := onMain(func() { rc = register(code, mods, uint64(id)<<32|hotKeySignature, appTarget, 0, &ref) })
	if !ok {
		// Registered after all, later, the key would be held by nobody's binding until the daemon stops.
		go func() {
			<-ran
			if rc == 0 {
				onMain(func() { unregister(ref) })
			}
		}()
		unavailable("the main thread did not answer")
		return
	}
	switch {
	case rc == eventHotKeyExistsErr:
		unavailable("another program holds the key")
		return
	case rc != 0:
		unavailable(fmt.Sprintf("macOS refused the key (%d)", rc))
		return
	}
	defer onMain(func() { unregister(ref) })
	ev.Status(Status{State: ipc.VoiceHotkeyRegistered})
	down := false
	for {
		select {
		case <-stop:
			return
		case d := <-events:
			if d != down {
				down = d
				ev.Held(d)
			}
		}
	}
}

// startApplication loads what the key needs and makes the application, on the main thread: Carbon's
// functions, the dispatch onto the main queue, and the handler every hot key's events reach. A daemon
// started outside the logged-in desktop, over SSH, has no WindowServer session, and is told so rather than
// asked to make an application there.
func startApplication() (objc.ID, error) {
	cg, err := purego.Dlopen(frameworks+"CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, errors.New("the CoreGraphics framework could not be loaded")
	}
	var sessionInfo func() uintptr
	purego.RegisterLibFunc(&sessionInfo, cg, "CGSessionCopyCurrentDictionary")
	if sessionInfo() == 0 {
		return 0, errors.New("the daemon runs outside the logged-in desktop, which the key needs")
	}
	carbon, err := purego.Dlopen(frameworks+"Carbon.framework/Carbon", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, errors.New("the Carbon framework could not be loaded")
	}
	if _, err := purego.Dlopen(frameworks+"AppKit.framework/AppKit", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
		return 0, errors.New("the AppKit framework could not be loaded")
	}
	system, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, errors.New("libSystem could not be loaded")
	}
	if mainQueue, err = purego.Dlsym(system, "_dispatch_main_q"); err != nil {
		return 0, errors.New("the main dispatch queue could not be found")
	}
	purego.RegisterLibFunc(&dispatchAsyncF, system, "dispatch_async_f")
	work = purego.NewCallback(func(ctx uintptr) {
		queuedMu.Lock()
		f := queued[ctx]
		delete(queued, ctx)
		queuedMu.Unlock()
		if f != nil {
			f()
		}
	})

	var getTarget func() uintptr
	var install func(target, handler uintptr, n uint32, types unsafe.Pointer, user uintptr, out *uintptr) int32
	var getKind func(ev uintptr) uint32
	var getParam func(ev uintptr, name, typ uint32, actual uintptr, size uintptr, outSize uintptr,
		out unsafe.Pointer) int32
	purego.RegisterLibFunc(&getTarget, carbon, "GetApplicationEventTarget")
	purego.RegisterLibFunc(&install, carbon, "InstallEventHandler")
	purego.RegisterLibFunc(&getKind, carbon, "GetEventKind")
	purego.RegisterLibFunc(&getParam, carbon, "GetEventParameter")
	purego.RegisterLibFunc(&register, carbon, "RegisterEventHotKey")
	purego.RegisterLibFunc(&unregister, carbon, "UnregisterEventHotKey")
	appTarget = getTarget()

	handler := purego.NewCallback(func(_, ev, _ uintptr) uintptr {
		var hk struct{ signature, id uint32 }
		if getParam(ev, kEventParamDirectObject, typeEventHotKeyID, 0, unsafe.Sizeof(hk), 0,
			unsafe.Pointer(&hk)) != 0 || hk.signature != hotKeySignature {
			return 0
		}
		keysMu.Lock()
		events := keys[hk.id]
		keysMu.Unlock()
		if events == nil {
			return 0
		}
		// Never blocking the main thread: a binding that fell this far behind loses a report rather than
		// stalling every event the application has.
		select {
		case events <- getKind(ev) == kEventHotKeyPressed:
		default:
		}
		return 0
	})
	types := [4]uint32{kEventClassKeyboard, kEventHotKeyPressed, kEventClassKeyboard, kEventHotKeyReleased}
	var ref uintptr
	if rc := install(appTarget, handler, 2, unsafe.Pointer(&types[0]), 0, &ref); rc != 0 {
		return 0, fmt.Errorf("the Carbon framework refused the key's handler (%d)", rc)
	}
	app := objc.ID(objc.GetClass("NSApplication")).Send(objc.RegisterName("sharedApplication"))
	if app == 0 {
		return 0, errors.New("the application could not be made")
	}
	app.Send(objc.RegisterName("setActivationPolicy:"), nsApplicationProhibited)
	return app, nil
}

// onMain runs f on the main thread and waits for it, at most mainThreadCallWithin, reporting whether it ran
// by then; ran is closed whenever it does.
func onMain(f func()) (ran <-chan struct{}, ok bool) {
	finished := make(chan struct{})
	queuedMu.Lock()
	nextQueued++
	ctx := nextQueued
	queued[ctx] = func() {
		defer close(finished)
		f()
	}
	queuedMu.Unlock()
	dispatchAsyncF(mainQueue, ctx, work)
	select {
	case <-finished:
		return finished, true
	case <-time.After(mainThreadCallWithin):
		return finished, false
	}
}
