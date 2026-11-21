// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"context"
	"fmt"
	"io"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// System is AudioToolbox's AudioQueue, called through purego with no cgo, as the daemon calls Carbon
// (ADR 0034): the default input and output, in 48 kHz mono 16-bit, which the queue converts to and from
// the device's own format. Its callbacks run on threads of AudioQueue's own, since no run loop is given.
//
// The microphone's use is charged to the daemon, the worker's parent (ADR 0034). Whether macOS allows it
// is asked of AVFoundation first (Authorize, avfoundation_darwin.go), and the queue is opened only on a
// yes: a refused microphone delivers silence here, so silence proves nothing.
//
// A queue whose device goes away stops calling back, and is opened again on the default device there is
// then (follow.go, M34). One still running is opened again too once CoreAudio names another default device
// than the one it opened on (M34's manual pass). Whether a queue left on the default follows a new default
// by itself is not known here: if it does, the reopening costs a moment's gap and nothing else. Written
// against Apple's documentation and cross-compiled, and not yet run on a Mac.
//
// Written at M29 against Apple's headers and cross-compiled; run on the author's Mac by the milestone's
// manual pass, which is the first time it ran.
func System() Device { return audioQueue{} }

type audioQueue struct{}

// streamDescription is AudioStreamBasicDescription.
type streamDescription struct {
	sampleRate       float64
	formatID         uint32
	formatFlags      uint32
	bytesPerPacket   uint32
	framesPerPacket  uint32
	bytesPerFrame    uint32
	channelsPerFrame uint32
	bitsPerChannel   uint32
	_                uint32 // mReserved
}

// queueBuffer is AudioQueueBuffer, as far as the fields this file reads.
type queueBuffer struct {
	capacity uint32
	data     unsafe.Pointer
	size     uint32
}

const (
	formatLinearPCM  = 0x6C70636D // 'lpcm'
	flagSignedInt    = 0x4
	flagPacked       = 0x8
	queueBuffers     = 3
	bytesPerBuffer   = FrameSamples * 2
	audioToolboxPath = "/System/Library/Frameworks/AudioToolbox.framework/AudioToolbox"
	coreAudioPath    = "/System/Library/Frameworks/CoreAudio.framework/CoreAudio"

	// The system object, and its default devices' properties ('dIn ', 'dOut', in 'glob', element main).
	systemObject      = 1
	propDefaultInput  = 0x64496E20
	propDefaultOutput = 0x644F7574
	scopeGlobal       = 0x676C6F62
	elementMain       = 0
)

// propertyAddress is AudioObjectPropertyAddress.
type propertyAddress struct {
	selector, scope, element uint32
}

var (
	loadOnce sync.Once
	loadErr  error

	newInput  func(format *streamDescription, cb uintptr, user uintptr, runLoop, mode uintptr, flags uint32, out *unsafe.Pointer) int32
	newOutput func(format *streamDescription, cb uintptr, user uintptr, runLoop, mode uintptr, flags uint32, out *unsafe.Pointer) int32
	allocate  func(q unsafe.Pointer, size uint32, out *unsafe.Pointer) int32
	enqueue   func(q, b unsafe.Pointer, descs uint32, desc uintptr) int32
	start     func(q unsafe.Pointer, at uintptr) int32
	stopQueue func(q unsafe.Pointer, immediate bool) int32
	dispose   func(q unsafe.Pointer, immediate bool) int32
	// getProperty is AudioObjectGetPropertyData, nil when CoreAudio could not be loaded: then a queue follows
	// only a device that goes away.
	getProperty func(obj uint32, addr *propertyAddress, qualSize uint32, qual uintptr, size *uint32, out unsafe.Pointer) int32

	// inputCallback and outputCallback are made once: purego never frees a callback, and a worker runs one
	// call. Each finds its queue by the id it was given as user data.
	inputCallback, outputCallback uintptr

	queuesMu sync.Mutex
	queues   = map[uintptr]*queue{}
	nextID   uintptr
)

func load() error {
	loadOnce.Do(func() {
		lib, err := purego.Dlopen(audioToolboxPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = fmt.Errorf("audio: loading AudioToolbox: %w", err)
			return
		}
		purego.RegisterLibFunc(&newInput, lib, "AudioQueueNewInput")
		purego.RegisterLibFunc(&newOutput, lib, "AudioQueueNewOutput")
		purego.RegisterLibFunc(&allocate, lib, "AudioQueueAllocateBuffer")
		purego.RegisterLibFunc(&enqueue, lib, "AudioQueueEnqueueBuffer")
		purego.RegisterLibFunc(&start, lib, "AudioQueueStart")
		purego.RegisterLibFunc(&stopQueue, lib, "AudioQueueStop")
		purego.RegisterLibFunc(&dispose, lib, "AudioQueueDispose")
		if ca, err := purego.Dlopen(coreAudioPath, purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
			purego.RegisterLibFunc(&getProperty, ca, "AudioObjectGetPropertyData")
		}
		inputCallback = purego.NewCallback(func(user uintptr, _ unsafe.Pointer, buf unsafe.Pointer, _ uintptr, _ uint32, _ uintptr) {
			if q := lookup(user); q != nil {
				q.captured(buf)
			}
		})
		outputCallback = purego.NewCallback(func(user uintptr, _ unsafe.Pointer, buf unsafe.Pointer) {
			if q := lookup(user); q != nil {
				q.render(buf)
			}
		})
	})
	return loadErr
}

// defaultDevice is CoreAudio's default input or output device now, and false when it cannot say.
func defaultDevice(input bool) (uint32, bool) {
	if getProperty == nil {
		return 0, false
	}
	addr := propertyAddress{selector: propDefaultOutput, scope: scopeGlobal, element: elementMain}
	if input {
		addr.selector = propDefaultInput
	}
	var dev uint32
	size := uint32(unsafe.Sizeof(dev))
	if st := getProperty(systemObject, &addr, 0, 0, &size, unsafe.Pointer(&dev)); st != 0 || dev == 0 {
		return 0, false
	}
	return dev, true
}

func lookup(id uintptr) *queue {
	queuesMu.Lock()
	defer queuesMu.Unlock()
	return queues[id]
}

// queue is one direction's AudioQueue and its buffers, which go round between the queue and the callback.
type queue struct {
	id     uintptr
	q      unsafe.Pointer
	mu     sync.Mutex
	closed bool
	framer *framer
	filler *filler
	// heard tells the follower the queue called back, and says whether it is the queue in use (follow.go).
	heard func() bool
	once  sync.Once
	// input is the queue's direction, and device the default device it opened on; known is false when
	// CoreAudio named none.
	input  bool
	device uint32
	known  bool
}

// Moved is whether CoreAudio names another default device than the one the queue opened on (follow.go's
// mover).
func (q *queue) Moved() bool {
	if !q.known {
		return false
	}
	dev, ok := defaultDevice(q.input)
	return ok && dev != q.device
}

func (q *queue) captured(buf unsafe.Pointer) {
	b := (*queueBuffer)(buf)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if q.heard() && b.size > 0 && b.data != nil {
		q.framer.write(unsafe.Slice((*int16)(b.data), b.size/2))
	}
	enqueue(q.q, buf, 0, 0)
}

func (q *queue) render(buf unsafe.Pointer) {
	b := (*queueBuffer)(buf)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if out := unsafe.Slice((*int16)(b.data), b.capacity/2); q.heard() {
		q.filler.read(out)
	} else {
		clear(out) // not the queue in use yet: silence
	}
	b.size = b.capacity
	enqueue(q.q, buf, 0, 0)
}

func (q *queue) Close() error {
	q.once.Do(func() {
		q.mu.Lock()
		q.closed = true
		q.mu.Unlock()
		// Stop and dispose wait for a callback in progress, which no longer holds anything once closed.
		stopQueue(q.q, true)
		dispose(q.q, true)
		queuesMu.Lock()
		delete(queues, q.id)
		queuesMu.Unlock()
	})
	return nil
}

var pcm16 = streamDescription{sampleRate: SampleRate, formatID: formatLinearPCM, formatFlags: flagSignedInt | flagPacked,
	bytesPerPacket: 2, framesPerPacket: 1, bytesPerFrame: 2, channelsPerFrame: 1, bitsPerChannel: 16}

func openQueue(input bool, q *queue) (io.Closer, error) {
	if err := load(); err != nil {
		return nil, err
	}
	// Read before the queue is made: a default that changes in between is one reopening more.
	q.input = input
	q.device, q.known = defaultDevice(input)
	queuesMu.Lock()
	nextID++
	q.id = nextID
	queues[q.id] = q
	queuesMu.Unlock()
	name, mk, cb := "the speakers", newOutput, outputCallback
	if input {
		name, mk, cb = "the microphone", newInput, inputCallback
	}
	format := pcm16
	if st := mk(&format, cb, q.id, 0, 0, 0, &q.q); st != 0 {
		queuesMu.Lock()
		delete(queues, q.id)
		queuesMu.Unlock()
		return nil, fmt.Errorf("audio: opening %s: OSStatus %d", name, st)
	}
	for range queueBuffers {
		var b unsafe.Pointer
		if st := allocate(q.q, bytesPerBuffer, &b); st != 0 {
			_ = q.Close()
			return nil, fmt.Errorf("audio: buffers for %s: OSStatus %d", name, st)
		}
		if input {
			enqueue(q.q, b, 0, 0)
		} else {
			q.render(b) // an output queue starts with its buffers full
		}
	}
	if st := start(q.q, 0); st != 0 {
		_ = q.Close()
		return nil, fmt.Errorf("audio: starting %s: OSStatus %d", name, st)
	}
	return q, nil
}

func (audioQueue) Capture(_ context.Context, frame func([]int16)) (io.Closer, error) {
	f := &framer{frame: frame}
	return follow(func(heard func() bool) (io.Closer, error) {
		return openQueue(true, &queue{framer: f, heard: heard})
	})
}

func (audioQueue) Play(_ context.Context, fill func([]int16)) (io.Closer, error) {
	f := newFiller(fill)
	return follow(func(heard func() bool) (io.Closer, error) {
		return openQueue(false, &queue{filler: f, heard: heard})
	})
}
