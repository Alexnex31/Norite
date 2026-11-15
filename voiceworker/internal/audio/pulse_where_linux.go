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
// microphone on auto_null's monitor, and speakers on auto_null, reach no device. Another sink's monitor is
// somebody's choice, desktop audio shared or a virtual cable, and is a microphone like any other. The
// samples decide nothing, since an idle card's are zeros too.
//
// It asks on a client of its own, never the stream's, kept for the stream's life and made again after a
// request fails: one made each second cost a connection and its handshake each time, and every client
// that comes and goes is announced to whatever watches the server, a desktop's volume applet included
// (M32's /optimization-review). askMu orders it with Close, which closes it once a question in progress
// has had its answer, so it is never used closed: the library's closed client may panic. The library
// keeps a stream's index to itself, so the stream is found by this process's id, which is unique: a worker
// has one stream each way. Anything that cannot be found or asked is nil, since a stream that cannot be
// seen may well be working, and a server that went away stops the stream's callbacks, which the session
// sees.
func (s *stream) Where() error {
	if !s.onStandIn() {
		return nil
	}
	// logind is asked outside askMu, so Close never waits on the system bus.
	what := "the speakers"
	if s.capture {
		what = "the microphone"
	}
	sessions, err := loginSessions()
	return stoodIn(what, sessions, err)
}

// onStandIn is whether the server has the stream on its stand-in for no device.
func (s *stream) onStandIn() bool {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	if s.closed {
		return false
	}
	if s.asker == nil {
		c, err := pulse.NewClient(pulse.ClientApplicationName("Norite"))
		if err != nil {
			return false
		}
		s.asker = c
	}
	c := s.asker
	failed := func() bool {
		c.Close()
		s.asker = nil
		return false
	}
	if s.capture {
		var outs proto.GetSourceOutputInfoListReply
		if c.RawRequest(&proto.GetSourceOutputInfoList{}, &outs) != nil {
			return failed()
		}
		i := own(len(outs), func(i int) proto.PropList { return outs[i].Properties })
		if i < 0 {
			return false
		}
		var src proto.GetSourceInfoReply
		if c.RawRequest(&proto.GetSourceInfo{SourceIndex: outs[i].SourceIndex}, &src) != nil {
			return failed()
		}
		return standInSource(src)
	}
	var ins proto.GetSinkInputInfoListReply
	if c.RawRequest(&proto.GetSinkInputInfoList{}, &ins) != nil {
		return failed()
	}
	i := own(len(ins), func(i int) proto.PropList { return ins[i].Properties })
	if i < 0 {
		return false
	}
	var sink proto.GetSinkInfoReply
	if c.RawRequest(&proto.GetSinkInfo{SinkIndex: ins[i].SinkIndex}, &sink) != nil {
		return failed()
	}
	return sink.SinkName == nullSink
}

// standInSource is whether src is the stand-in for no input: auto_null's monitor. In a source's reply the
// library's MonitorSourceIndex and MonitorSourceName are the protocol's monitor_of_sink and
// monitor_of_sink_name, the sink it is the monitor of, so one request answers.
func standInSource(src proto.GetSourceInfoReply) bool {
	return src.MonitorSourceIndex != proto.Undefined && src.MonitorSourceName == nullSink
}

// nullSink is the name a PulseAudio server gives its stand-in for no output, PipeWire's and
// module-always-sink's alike. Its monitor is the stand-in for no input, auto_null.monitor in M32's spike,
// and is known by the sink it monitors rather than by its own name.
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
