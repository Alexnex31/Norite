// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// System is WASAPI in shared mode, through COM called from Go with no cgo: the default devices, asked for
// 48 kHz mono 16-bit, which the audio engine converts to and from whatever the device runs at
// (AUTOCONVERTPCM). Windows names the worker's executable in its microphone switch (ADR 0034); with the
// switch off, activating the microphone is refused, and the error is a Refused Failure (outcome.go). A
// device that goes away mid-call stops the engine's events, its calls answering AUDCLNT_E_DEVICE_INVALIDATED,
// and the stream is opened again on the default device there is then (follow.go, M34). A stream that is
// still running asks every followEvery, on its own thread, which endpoint is the default, and says it moved
// once that is not the one it opened, so a headset plugged in mid-call takes the call (M34's manual pass).
// Windows is not asked to notify of changes (IMMNotificationClient): that would be a COM object written in
// Go, called on threads of Windows', where four questions a second on a thread the stream already has
// cost nothing.
//
// Written at M29 against Microsoft's documentation and cross-compiled; run in M24's Windows VM by the
// milestone's manual pass, which is the first time it ran.
func System() Device { return wasapi{} }

type wasapi struct{}

var (
	ole32                = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procCreateEventW     = kernel32.NewProc("CreateEventW")
	procWaitForSingle    = kernel32.NewProc("WaitForSingleObject")
)

type guid struct {
	d1     uint32
	d2, d3 uint16
	d4     [8]byte
}

var (
	clsidMMDeviceEnumerator = guid{0xBCDE0395, 0xE52F, 0x467C, [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator  = guid{0xA95664D2, 0x9614, 0x4F35, [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioClient         = guid{0x1CB9AD4C, 0xDBFA, 0x4C32, [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient  = guid{0xC8ADBD64, 0xE71E, 0x48A0, [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
	iidIAudioRenderClient   = guid{0xF294ACFC, 0x3146, 0x4483, [8]byte{0xA7, 0xBF, 0xAD, 0xDC, 0xA7, 0xC2, 0x60, 0xE2}}
)

const (
	coinitMultithreaded = 0x0
	clsctxAll           = 0x17
	eRender, eCapture   = 0, 1
	eConsole            = 0

	sharemodeShared       = 0
	streamflagsEvent      = 0x00040000
	streamflagsAutoPCM    = 0x80000000
	streamflagsSRCQuality = 0x08000000
	bufferflagsSilent     = 0x2
	// bufferDuration is what each direction asks the engine to hold, in 100 ns units: 40 ms, two frames.
	bufferDuration = 400_000
	waitTimeout    = 0x102
	sOK, sFalse    = 0, 1
)

// Vtable indices, in the order each interface declares its methods after IUnknown's three.
const (
	mmeGetDefaultAudioEndpoint = 4
	mmdActivate                = 3
	mmdGetID                   = 5
	acInitialize               = 3
	acGetBufferSize            = 4
	acGetCurrentPadding        = 6
	acStart                    = 10
	acStop                     = 11
	acSetEventHandle           = 13
	acGetService               = 14
	ccGetBuffer                = 3
	ccReleaseBuffer            = 4
	ccGetNextPacketSize        = 5
	rcGetBuffer                = 3
	rcReleaseBuffer            = 4
	unkRelease                 = 2
)

type waveFormatEx struct {
	formatTag      uint16
	channels       uint16
	samplesPerSec  uint32
	avgBytesPerSec uint32
	blockAlign     uint16
	bitsPerSample  uint16
	size           uint16
}

// com calls method idx of obj, a COM interface pointer, and returns its HRESULT.
func com(obj unsafe.Pointer, idx int, args ...uintptr) uint32 {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, idx*int(unsafe.Sizeof(uintptr(0)))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return uint32(r)
}

func release(obj unsafe.Pointer) {
	if obj != nil {
		com(obj, unkRelease)
	}
}

// stream is one direction's client, run on a thread of its own: COM objects are used where they were
// made, and the thread is locked so that is always the same one.
type stream struct {
	once  sync.Once
	stop  chan struct{}
	done  chan struct{}
	moved atomic.Bool
}

// Moved is whether another endpoint has become the default since the stream opened (follow.go's mover).
func (s *stream) Moved() bool { return s.moved.Load() }

func (s *stream) Close() error {
	s.once.Do(func() { close(s.stop) })
	<-s.done
	return nil
}

// open starts a stream's thread, which reports whether it opened before it starts running.
func open(dir int, run func(client, service unsafe.Pointer, l *loop)) (io.Closer, error) {
	s := &stream{stop: make(chan struct{}), done: make(chan struct{})}
	opened := make(chan error, 1)
	go func() {
		defer close(s.done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded); uint32(hr) != sOK && uint32(hr) != sFalse {
			opened <- hresult("starting COM", uint32(hr))
			return
		}
		defer procCoUninitialize.Call() //nolint:errcheck // nothing to do with its answer
		client, service, event, at, err := activate(dir)
		if err != nil {
			opened <- err
			return
		}
		defer release(at.enum)
		defer release(client)
		defer release(service)
		defer syscall.CloseHandle(syscall.Handle(event)) //nolint:errcheck // closing on the way out
		if hr := com(client, acStart); hr != sOK {
			opened <- hresult("starting the stream", hr)
			return
		}
		defer com(client, acStop)
		opened <- nil
		run(client, service, &loop{event: event, stop: s.stop, at: at, moved: &s.moved})
	}()
	if err := <-opened; err != nil {
		<-s.done
		return nil, err
	}
	return s, nil
}

// endpoint is the device a stream opened, by its id, and the enumerator that asks which is the default now,
// both used on the stream's thread alone.
type endpoint struct {
	enum unsafe.Pointer
	flow uintptr
	id   string
}

// defaultID is the id of the default endpoint for the stream's direction, and false when there is none or
// Windows cannot say.
func (e endpoint) defaultID() (string, bool) {
	dev := new(unsafe.Pointer)
	if hr := com(e.enum, mmeGetDefaultAudioEndpoint, e.flow, eConsole, uintptr(unsafe.Pointer(dev))); hr != sOK {
		return "", false
	}
	defer release(*dev)
	return deviceID(*dev)
}

// deviceID is IMMDevice::GetId's string, which COM allocated and is freed here.
func deviceID(dev unsafe.Pointer) (string, bool) {
	var p *uint16
	if hr := com(dev, mmdGetID, uintptr(unsafe.Pointer(&p))); hr != sOK || p == nil {
		return "", false
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p))) //nolint:errcheck // CoTaskMemFree returns nothing
	// An endpoint id is some sixty characters; one past maxDeviceID is cut there, which a comparison of two
	// such ids still answers.
	const maxDeviceID = 1024
	var u []uint16
	for q := unsafe.Pointer(p); len(u) < maxDeviceID; q = unsafe.Add(q, 2) {
		c := *(*uint16)(q)
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return syscall.UTF16ToString(u), true
}

// loop is a stream's waiting for the engine, which asks which endpoint is the default every followEvery
// while it waits, and says the stream moved once it is another than the stream's.
type loop struct {
	event uintptr
	stop  <-chan struct{}
	at    endpoint
	moved *atomic.Bool
	next  time.Time
}

// wait blocks until the engine signals or stop closes; false means stop.
func (l *loop) wait() bool {
	for {
		select {
		case <-l.stop:
			return false
		default:
		}
		if now := time.Now(); now.After(l.next) {
			l.next = now.Add(followEvery)
			if id, ok := l.at.defaultID(); ok && l.at.id != "" && id != l.at.id {
				l.moved.Store(true)
			}
		}
		r, _, _ := procWaitForSingle.Call(l.event, 100)
		if uint32(r) != waitTimeout {
			return true
		}
	}
}

// activate makes the default device's client for dir, in 48 kHz mono 16-bit, driven by an event. The
// endpoint it returns holds the enumerator, which the caller releases.
func activate(dir int) (client, service unsafe.Pointer, event uintptr, at endpoint, err error) {
	var enum unsafe.Pointer
	if hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum))); uint32(hr) != sOK {
		return nil, nil, 0, endpoint{}, hresult("finding the audio devices", uint32(hr))
	}
	kept := false
	defer func() {
		if !kept {
			release(enum)
		}
	}()
	name := "the speakers"
	flow := uintptr(eRender)
	svc := &iidIAudioRenderClient
	if dir == eCapture {
		name, flow, svc = "the microphone", eCapture, &iidIAudioCaptureClient
	}
	dev := new(unsafe.Pointer)
	if hr := com(enum, mmeGetDefaultAudioEndpoint, flow, eConsole, uintptr(unsafe.Pointer(dev))); hr != sOK {
		return nil, nil, 0, endpoint{}, hresult("finding "+name, hr)
	}
	defer release(*dev)
	cl := new(unsafe.Pointer)
	if hr := com(*dev, mmdActivate, uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0,
		uintptr(unsafe.Pointer(cl))); hr != sOK {
		return nil, nil, 0, endpoint{}, hresult("opening "+name, hr)
	}
	format := &waveFormatEx{formatTag: 1, channels: 1, samplesPerSec: SampleRate, avgBytesPerSec: SampleRate * 2,
		blockAlign: 2, bitsPerSample: 16}
	if hr := com(*cl, acInitialize, sharemodeShared, streamflagsEvent|streamflagsAutoPCM|streamflagsSRCQuality,
		bufferDuration, 0, uintptr(unsafe.Pointer(format)), 0); hr != sOK {
		release(*cl)
		return nil, nil, 0, endpoint{}, hresult("opening "+name+" at 48 kHz", hr)
	}
	ev, _, _ := procCreateEventW.Call(0, 0, 0, 0)
	if ev == 0 {
		release(*cl)
		return nil, nil, 0, endpoint{}, errors.New("audio: no event could be made for " + name)
	}
	if hr := com(*cl, acSetEventHandle, ev); hr != sOK {
		release(*cl)
		_ = syscall.CloseHandle(syscall.Handle(ev))
		return nil, nil, 0, endpoint{}, hresult("driving "+name, hr)
	}
	sv := new(unsafe.Pointer)
	if hr := com(*cl, acGetService, uintptr(unsafe.Pointer(svc)), uintptr(unsafe.Pointer(sv))); hr != sOK {
		release(*cl)
		_ = syscall.CloseHandle(syscall.Handle(ev))
		return nil, nil, 0, endpoint{}, hresult("reaching "+name, hr)
	}
	runtime.KeepAlive(format)
	// An id Windows would not give leaves the stream following a device that goes away, and no other.
	id, _ := deviceID(*dev)
	kept = true
	return *cl, *sv, ev, endpoint{enum: enum, flow: flow, id: id}, nil
}

func (wasapi) Capture(_ context.Context, frame func([]int16)) (io.Closer, error) {
	f := &framer{frame: frame}
	return follow(func(heard func() bool) (io.Closer, error) { return captureStream(f, heard) })
}

func captureStream(f *framer, heard func() bool) (io.Closer, error) {
	silent := make([]int16, SampleRate/10)
	return open(eCapture, func(_, cc unsafe.Pointer, l *loop) {
		data, frames, flags := new(*int16), new(uint32), new(uint32)
		next := new(uint32)
		for l.wait() {
			for com(cc, ccGetNextPacketSize, uintptr(unsafe.Pointer(next))) == sOK && *next > 0 {
				if com(cc, ccGetBuffer, uintptr(unsafe.Pointer(data)), uintptr(unsafe.Pointer(frames)),
					uintptr(unsafe.Pointer(flags)), 0, 0) != sOK {
					break
				}
				n := int(*frames)
				switch {
				case !heard():
					// Not the stream in use yet: the one before it still feeds the call (follow.go).
				case *flags&bufferflagsSilent != 0 || *data == nil:
					for n > 0 {
						c := min(n, len(silent))
						f.write(silent[:c])
						n -= c
					}
				default:
					f.write(unsafe.Slice(*data, n))
				}
				com(cc, ccReleaseBuffer, uintptr(*frames))
			}
		}
	})
}

func (wasapi) Play(_ context.Context, fill func([]int16)) (io.Closer, error) {
	f := newFiller(fill)
	return follow(func(heard func() bool) (io.Closer, error) { return playStream(f, heard) })
}

func playStream(f *filler, heard func() bool) (io.Closer, error) {
	return open(eRender, func(client, rc unsafe.Pointer, l *loop) {
		total, padding := new(uint32), new(uint32)
		if com(client, acGetBufferSize, uintptr(unsafe.Pointer(total))) != sOK {
			return
		}
		data := new(*int16)
		for l.wait() {
			if com(client, acGetCurrentPadding, uintptr(unsafe.Pointer(padding))) != sOK {
				continue
			}
			avail := *total - *padding
			// The engine asked: the device is there, whether or not it has room for a frame now.
			use := heard()
			if avail == 0 || com(rc, rcGetBuffer, uintptr(avail), uintptr(unsafe.Pointer(data))) != sOK {
				continue
			}
			if out := unsafe.Slice(*data, int(avail)); use {
				f.read(out)
			} else {
				clear(out) // not the stream in use yet (follow.go): silence
			}
			com(rc, rcReleaseBuffer, uintptr(avail), 0)
		}
	})
}
