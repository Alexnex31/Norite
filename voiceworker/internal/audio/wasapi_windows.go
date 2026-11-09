// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// System is WASAPI in shared mode, through COM called from Go with no cgo: the default devices, asked for
// 48 kHz mono 16-bit, which the audio engine converts to and from whatever the device runs at
// (AUTOCONVERTPCM). Windows names the worker's executable in its microphone switch (ADR 0034); with the
// switch off, activating the microphone is refused and the reason says so.
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

func hresult(what string, hr uint32) error {
	switch hr {
	case 0x80070005:
		return fmt.Errorf("audio: %s: access is denied (Settings, Privacy, Microphone)", what)
	case 0x88890004:
		return fmt.Errorf("audio: %s: the device was removed", what)
	case 0x80070490:
		return fmt.Errorf("audio: %s: no such device", what)
	}
	return fmt.Errorf("audio: %s: HRESULT 0x%08x", what, hr)
}

// stream is one direction's client, run on a thread of its own: COM objects are used where they were
// made, and the thread is locked so that is always the same one.
type stream struct {
	once sync.Once
	stop chan struct{}
	done chan struct{}
}

func (s *stream) Close() error {
	s.once.Do(func() { close(s.stop) })
	<-s.done
	return nil
}

// open starts a stream's thread, which reports whether it opened before it starts running.
func open(dir int, run func(client, service unsafe.Pointer, event uintptr, stop <-chan struct{})) (io.Closer, error) {
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
		client, service, event, err := activate(dir)
		if err != nil {
			opened <- err
			return
		}
		defer release(client)
		defer release(service)
		defer syscall.CloseHandle(syscall.Handle(event)) //nolint:errcheck // closing on the way out
		if hr := com(client, acStart); hr != sOK {
			opened <- hresult("starting the stream", hr)
			return
		}
		defer com(client, acStop)
		opened <- nil
		run(client, service, event, s.stop)
	}()
	if err := <-opened; err != nil {
		<-s.done
		return nil, err
	}
	return s, nil
}

// activate makes the default device's client for dir, in 48 kHz mono 16-bit, driven by an event.
func activate(dir int) (client, service unsafe.Pointer, event uintptr, err error) {
	var enum unsafe.Pointer
	if hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum))); uint32(hr) != sOK {
		return nil, nil, 0, hresult("finding the audio devices", uint32(hr))
	}
	defer release(enum)
	name := "the speakers"
	flow := uintptr(eRender)
	svc := &iidIAudioRenderClient
	if dir == eCapture {
		name, flow, svc = "the microphone", eCapture, &iidIAudioCaptureClient
	}
	dev := new(unsafe.Pointer)
	if hr := com(enum, mmeGetDefaultAudioEndpoint, flow, eConsole, uintptr(unsafe.Pointer(dev))); hr != sOK {
		return nil, nil, 0, hresult("finding "+name, hr)
	}
	defer release(*dev)
	cl := new(unsafe.Pointer)
	if hr := com(*dev, mmdActivate, uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0,
		uintptr(unsafe.Pointer(cl))); hr != sOK {
		return nil, nil, 0, hresult("opening "+name, hr)
	}
	format := &waveFormatEx{formatTag: 1, channels: 1, samplesPerSec: SampleRate, avgBytesPerSec: SampleRate * 2,
		blockAlign: 2, bitsPerSample: 16}
	if hr := com(*cl, acInitialize, sharemodeShared, streamflagsEvent|streamflagsAutoPCM|streamflagsSRCQuality,
		bufferDuration, 0, uintptr(unsafe.Pointer(format)), 0); hr != sOK {
		release(*cl)
		return nil, nil, 0, hresult("opening "+name+" at 48 kHz", hr)
	}
	ev, _, _ := procCreateEventW.Call(0, 0, 0, 0)
	if ev == 0 {
		release(*cl)
		return nil, nil, 0, errors.New("audio: no event could be made for " + name)
	}
	if hr := com(*cl, acSetEventHandle, ev); hr != sOK {
		release(*cl)
		_ = syscall.CloseHandle(syscall.Handle(ev))
		return nil, nil, 0, hresult("driving "+name, hr)
	}
	sv := new(unsafe.Pointer)
	if hr := com(*cl, acGetService, uintptr(unsafe.Pointer(svc)), uintptr(unsafe.Pointer(sv))); hr != sOK {
		release(*cl)
		_ = syscall.CloseHandle(syscall.Handle(ev))
		return nil, nil, 0, hresult("reaching "+name, hr)
	}
	runtime.KeepAlive(format)
	return *cl, *sv, ev, nil
}

// wait blocks until the engine signals or stop closes; false means stop.
func wait(event uintptr, stop <-chan struct{}) bool {
	for {
		select {
		case <-stop:
			return false
		default:
		}
		r, _, _ := procWaitForSingle.Call(event, 100)
		if uint32(r) != waitTimeout {
			return true
		}
	}
}

func (wasapi) Capture(_ context.Context, frame func([]int16)) (io.Closer, error) {
	f := &framer{frame: frame}
	silent := make([]int16, SampleRate/10)
	return open(eCapture, func(_, cc unsafe.Pointer, event uintptr, stop <-chan struct{}) {
		data, frames, flags := new(*int16), new(uint32), new(uint32)
		next := new(uint32)
		for wait(event, stop) {
			for com(cc, ccGetNextPacketSize, uintptr(unsafe.Pointer(next))) == sOK && *next > 0 {
				if com(cc, ccGetBuffer, uintptr(unsafe.Pointer(data)), uintptr(unsafe.Pointer(frames)),
					uintptr(unsafe.Pointer(flags)), 0, 0) != sOK {
					break
				}
				n := int(*frames)
				if *flags&bufferflagsSilent != 0 || *data == nil {
					for n > 0 {
						c := min(n, len(silent))
						f.write(silent[:c])
						n -= c
					}
				} else {
					f.write(unsafe.Slice(*data, n))
				}
				com(cc, ccReleaseBuffer, uintptr(*frames))
			}
		}
	})
}

func (wasapi) Play(_ context.Context, fill func([]int16)) (io.Closer, error) {
	f := newFiller(fill)
	return open(eRender, func(client, rc unsafe.Pointer, event uintptr, stop <-chan struct{}) {
		total, padding := new(uint32), new(uint32)
		if com(client, acGetBufferSize, uintptr(unsafe.Pointer(total))) != sOK {
			return
		}
		data := new(*int16)
		for wait(event, stop) {
			if com(client, acGetCurrentPadding, uintptr(unsafe.Pointer(padding))) != sOK {
				continue
			}
			avail := *total - *padding
			if avail == 0 || com(rc, rcGetBuffer, uintptr(avail), uintptr(unsafe.Pointer(data))) != sOK {
				continue
			}
			f.read(unsafe.Slice(*data, int(avail)))
			com(rc, rcReleaseBuffer, uintptr(avail), 0)
		}
	})
}
