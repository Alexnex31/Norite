// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"os"
	"strconv"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

// Where is whether the stream reaches a device (M32's spike, PipeWire 1.4.2). With nobody logged in at a
// seat, a PulseAudio server says yes to everything: the default source is the monitor of a stand-in sink,
// auto_null, recording delivers zeros and playback consumes. The session ending moves an open stream
// there, and the next login moves it back, both silently. So what decides is where the stream is now: a
// microphone on a monitor, of any sink, records no person, and speakers on auto_null play to nobody. The
// samples decide nothing, since an idle card's are zeros too.
//
// It asks on a client of its own, made and closed each time, never the stream's: the library's closed
// client may panic, and Close may run at any moment. The library keeps a stream's index to itself, so the
// stream is found by this process's id, which is unique: a worker has one stream each way. Anything that
// cannot be found or asked is nil, since a stream that cannot be seen may well be working, and a server
// that went away stops the stream's callbacks, which the session sees.
func (s *stream) Where() error {
	c, err := pulse.NewClient(pulse.ClientApplicationName("Norite"))
	if err != nil {
		return nil
	}
	defer c.Close()
	if s.capture {
		var outs proto.GetSourceOutputInfoListReply
		if c.RawRequest(&proto.GetSourceOutputInfoList{}, &outs) != nil {
			return nil
		}
		i := own(len(outs), func(i int) proto.PropList { return outs[i].Properties })
		if i < 0 {
			return nil
		}
		var src proto.GetSourceInfoReply
		if c.RawRequest(&proto.GetSourceInfo{SourceIndex: outs[i].SourceIndex}, &src) != nil ||
			src.MonitorSourceIndex == proto.Undefined {
			return nil
		}
		sessions, err := loginSessions()
		return stoodIn("the microphone", sessions, err)
	}
	var ins proto.GetSinkInputInfoListReply
	if c.RawRequest(&proto.GetSinkInputInfoList{}, &ins) != nil {
		return nil
	}
	i := own(len(ins), func(i int) proto.PropList { return ins[i].Properties })
	if i < 0 {
		return nil
	}
	var sink proto.GetSinkInfoReply
	if c.RawRequest(&proto.GetSinkInfo{SinkIndex: ins[i].SinkIndex}, &sink) != nil || sink.SinkName != nullSink {
		return nil
	}
	sessions, err := loginSessions()
	return stoodIn("the speakers", sessions, err)
}

// nullSink is the name a PulseAudio server gives its stand-in for no output, PipeWire's and
// module-always-sink's alike. A source that is a sink's monitor is no microphone, whichever sink, so the
// source needs no name.
const nullSink = "auto_null"

// own is the one of n streams whose properties name this process, or -1 when none does or several do.
func own(n int, props func(int) proto.PropList) int {
	pid, found := strconv.Itoa(os.Getpid()), -1
	for i := range n {
		if p, ok := props(i)["application.process.id"]; ok && p.String() == pid {
			if found >= 0 {
				return -1
			}
			found = i
		}
	}
	return found
}
