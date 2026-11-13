// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/rtcp"
)

// feedbackFor is a transport-wide feedback for packets 0 to n-1, received 20 ms apart but for every fifth,
// as the SFU's interceptor builds it.
func feedbackFor(t *testing.T, n int) []byte {
	t.Helper()
	rec := twcc.NewRecorder(1)
	for i := range n {
		if i%5 != 2 {
			rec.Record(7, uint16(i), int64(1_000_000+i*20_000)) //nolint:gosec // a test's count
		}
	}
	b, err := rtcp.Marshal(rec.BuildFeedbackPacket())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// hostileFeedback claims 8,191 received packets in one run-length chunk, and carries no delta for any.
func hostileFeedback() []byte {
	b := make([]byte, 24)
	b[0], b[1] = 0x80|15, 205      // version 2, FMT 15; transport-layer feedback
	b[3] = 24/4 - 1                // its length, in words, less one
	b[14], b[15] = 0x1f, 0xff      // 8,191 statuses
	b[20], b[21] = 0x20|0x1f, 0xff // a run of 8,191 received with a small delta
	return b
}

// What Pion would refuse after allocating a delta for each claimed packet is dropped before it is parsed;
// what the SFU's own interceptor builds, and a sender report, go through.
func TestOnlyAWholeFeedbackReachesPion(t *testing.T) {
	if !plausible(feedbackFor(t, 5)) || !plausible(feedbackFor(t, 300)) {
		t.Error("a feedback pion built was refused")
	}
	sr, err := (&rtcp.SenderReport{SSRC: 1}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !plausible(sr) || !plausible(append(sr, feedbackFor(t, 5)...)) {
		t.Error("a sender report was refused")
	}
	if plausible(hostileFeedback()) || plausible(append(sr, hostileFeedback()...)) {
		t.Error("a feedback claiming 8,191 deltas in no bytes was let through")
	}
	if _, err := rtcp.Unmarshal(hostileFeedback()); err == nil {
		t.Error("pion parsed the hostile feedback, so refusing it refuses something real")
	}
	whole := feedbackFor(t, 5)
	if plausible(whole[:len(whole)-4]) {
		t.Error("a truncated feedback was let through")
	}
}

type scripted struct {
	reads []func(b []byte) (int, error)
}

func (s *scripted) Read(b []byte, _ interceptor.Attributes) (int, interceptor.Attributes, error) {
	if len(s.reads) == 0 {
		return 0, nil, io.EOF
	}
	r := s.reads[0]
	s.reads = s.reads[1:]
	n, err := r(b)
	return n, nil, err
}

func packet(p []byte) func([]byte) (int, error) {
	return func(b []byte) (int, error) { return copy(b, p), nil }
}

// The guard sits under Pion's own interceptors and reads past what it drops.
func TestTheGuardReadsPastWhatItDrops(t *testing.T) {
	good := feedbackFor(t, 5)
	r := (&rtcpGuard{}).BindRTCPReader(&scripted{reads: []func([]byte) (int, error){
		packet(hostileFeedback()), packet(good)}})
	b := make([]byte, 1500)
	n, _, err := r.Read(b, nil)
	if err != nil || n != len(good) {
		t.Errorf("the guard returned %d bytes and %v, want the good feedback's %d", n, err, len(good))
	}
}

// The guard is under Pion's report interceptors, which parse every packet in full inside the read: read
// through the call's whole chain, the hostile feedback never reaches them, and the good one does. With the
// guard above them, the first read fails with Pion's own parse error, after its allocation.
func TestTheGuardIsUnderPionsInterceptors(t *testing.T) {
	reg, err := interceptors()
	if err != nil {
		t.Fatal(err)
	}
	chain, err := reg.Build("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = chain.Close() }()
	good := feedbackFor(t, 5)
	r := chain.BindRTCPReader(&scripted{reads: []func([]byte) (int, error){
		packet(hostileFeedback()), packet(good)}})
	b := make([]byte, 1500)
	n, _, err := r.Read(b, nil)
	if err != nil || n != len(good) {
		t.Errorf("the chain returned %d bytes and %v, want the good feedback's %d", n, err, len(good))
	}
}

type readerFunc func(b []byte) (int, interceptor.Attributes, error)

func (f readerFunc) Read(b []byte) (int, interceptor.Attributes, error) { return f(b) }

// tooShort is a sender report whose header is whole and whose body is not there: the guard's bounds pass
// it, and Pion's parser refuses it.
func tooShort() []byte { return []byte{0x80, 200, 0, 1, 0, 0, 0, 1} }

// A packet Pion could not parse costs that packet: the guard drops it, the reader goes on, and the next
// feedback reaches the controller. M28's drain returned on any error, which would have left the bitrate
// where it was for the rest of the call.
func TestAMalformedPacketDoesNotStopTheFeedback(t *testing.T) {
	if !plausible(tooShort()) {
		t.Fatal("the malformed packet does not reach the parse")
	}
	if _, err := rtcp.Unmarshal(tooShort()); err == nil {
		t.Fatal("pion parsed the malformed packet")
	}
	s := &Session{cc: testController(), opts: Options{Log: io.Discard}}
	now := time.Now()
	for seq := range uint16(20) {
		s.cc.Sent(seq, now)
	}
	var reads []func([]byte) (int, error)
	for range 30 {
		reads = append(reads, packet(tooShort()))
	}
	// Through the call's whole chain: Pion's report interceptor, above the guard, fails the read on a
	// packet it cannot parse.
	reg, err := interceptors()
	if err != nil {
		t.Fatal(err)
	}
	chain, err := reg.Build("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = chain.Close() }()
	r := chain.BindRTCPReader(&scripted{reads: append(reads, packet(feedbackFor(t, 20)))})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.readRTCP(readerFunc(func(b []byte) (int, interceptor.Attributes, error) { return r.Read(b, nil) }))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("readRTCP did not return at the end of the stream")
	}
	if l, measured := s.cc.Loss(); !measured || l < 0.19 || l > 0.21 {
		t.Errorf("after 30 malformed packets the controller measured %.0f%% loss (measured: %v), want the "+
			"feedback's 20%%", 100*l, measured)
	}
}

// A read that fails is the stream's, since the guard drops every packet that does not parse: reading
// stops. A stream whose transport closed before it opened fails every read without blocking, and reading
// on past it woke a thousand times a second for the rest of the call (M31's third /code-review).
func TestAStreamThatFailsEndsTheRead(t *testing.T) {
	var log strings.Builder
	s := &Session{cc: testController(), opts: Options{Log: &log}}
	reads := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.readRTCP(readerFunc(func([]byte) (int, interceptor.Attributes, error) {
			reads++
			return 0, nil, errors.New("failed type assertion")
		}))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("readRTCP read on past a stream that fails every read")
	}
	if reads != 1 {
		t.Errorf("readRTCP read %d times from a stream that failed the first", reads)
	}
	// And says so: the close's line would otherwise read as a controller that ran throughout.
	if !strings.Contains(log.String(), "RTCP could not be read (failed type assertion)") {
		t.Errorf("the log does not say the feedback ended: %q", log.String())
	}
}
