// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"context"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// Authorize asks AVFoundation whether the microphone may be used, and asks the person when macOS has not
// yet (ADR 0034 §1): AVCaptureDevice's authorizationStatusForMediaType:, then
// requestAccessForMediaType:completionHandler:. The prompt names norite-daemon, which the worker's use is
// charged to, and comes once per daemon build until releases carry a Developer ID (M32's entry). A refusal
// is never asked again in a loop: the person acts, and the next join asks again.
//
// AVFoundation calls the handler on a queue of its own, so no run loop is given. Written at M32 against
// Apple's documentation and cross-compiled; run on the author's Mac by the milestone's manual pass, which
// is the first time it ran.
func (audioQueue) Authorize(ctx context.Context, asking func()) error {
	if err := loadAV(); err != nil {
		return err
	}
	err := authorization(objc.Send[int](objc.ID(captureDevice), selStatus, mediaAudio))
	if err != errAsk { //nolint:errorlint // a sentinel of this package's, never wrapped
		return err
	}
	answer := make(chan bool, 1)
	handler := objc.NewBlock(func(_ objc.Block, yes bool) {
		select {
		case answer <- yes:
		default:
		}
	})
	// AVFoundation copies the handler; this copy is released here, and its own when it has run.
	defer handler.Release()
	asking()
	objc.ID(captureDevice).Send(selRequest, mediaAudio, handler)
	select {
	case yes := <-answer:
		return granted(yes)
	case <-ctx.Done():
		return ctx.Err()
	}
}

const avFoundationPath = "/System/Library/Frameworks/AVFoundation.framework/AVFoundation"

var (
	avOnce        sync.Once
	avErr         error
	captureDevice objc.Class
	mediaAudio    objc.ID // AVMediaTypeAudio
	selStatus     = objc.RegisterName("authorizationStatusForMediaType:")
	selRequest    = objc.RegisterName("requestAccessForMediaType:completionHandler:")
)

func loadAV() error {
	avOnce.Do(func() {
		lib, err := purego.Dlopen(avFoundationPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			avErr = fmt.Errorf("audio: loading AVFoundation: %w", err)
			return
		}
		sym, err := purego.Dlsym(lib, "AVMediaTypeAudio")
		if err != nil {
			avErr = fmt.Errorf("audio: finding AVMediaTypeAudio: %w", err)
			return
		}
		// sym is the address of the constant, an NSString pointer; read through the address of sym, so that
		// no integer becomes a pointer.
		mediaAudio = **(**objc.ID)(unsafe.Pointer(&sym))
		if captureDevice = objc.GetClass("AVCaptureDevice"); captureDevice == 0 || mediaAudio == 0 {
			avErr = fmt.Errorf("audio: AVFoundation has no AVCaptureDevice or AVMediaTypeAudio")
		}
	})
	return avErr
}
