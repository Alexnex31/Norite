// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package events

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"
)

// InProc is the single-process bus: every publisher and subscriber shares this value.
type InProc struct {
	logger *zerolog.Logger

	mu     sync.RWMutex
	subs   map[string]map[*inprocSub]struct{}
	closed bool
}

// NewInProc returns an empty in-process bus. logger records dropped messages and may be nil.
func NewInProc(logger *zerolog.Logger) *InProc {
	if logger == nil {
		nop := zerolog.Nop()
		logger = &nop
	}
	return &InProc{logger: logger, subs: map[string]map[*inprocSub]struct{}{}}
}

// Publish copies payload once and offers it to each subscriber without blocking.
func (b *InProc) Publish(_ context.Context, topic string, payload []byte) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return ErrClosed
	}
	subs := b.subs[topic]
	if len(subs) == 0 {
		return nil
	}

	// Copied because the publisher owns its slice and may reuse it the moment Publish returns, while a
	// subscriber reads this one later on its own goroutine. One copy is shared by every subscriber, which
	// is why handlers must not modify a payload.
	msg := append([]byte(nil), payload...)
	for s := range subs {
		select {
		case s.queue <- msg:
		default:
			// Dropped rather than blocking the publisher: see the package comment.
			if s.dropped.Add(1) == 1 {
				b.logger.Warn().Str("topic", topic).Msg("event subscriber is not keeping up; dropping messages")
			}
		}
	}
	return nil
}

// Subscribe registers handle for topic.
func (b *InProc) Subscribe(topic string, handle func(payload []byte)) (Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}

	s := &inprocSub{
		bus:   b,
		topic: topic,
		queue: make(chan []byte, subscriptionBuffer),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	if b.subs[topic] == nil {
		b.subs[topic] = map[*inprocSub]struct{}{}
	}
	b.subs[topic][s] = struct{}{}

	go s.deliver(handle)
	return s, nil
}

// Close stops every subscription. It waits for handlers in progress to return.
func (b *InProc) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	var all []*inprocSub
	for _, subs := range b.subs {
		for s := range subs {
			all = append(all, s)
		}
	}
	b.subs = map[string]map[*inprocSub]struct{}{}
	b.mu.Unlock()

	for _, s := range all {
		s.halt()
	}
	return nil
}

type inprocSub struct {
	bus   *InProc
	topic string
	queue chan []byte

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	dropped atomic.Int64
}

func (s *inprocSub) deliver(handle func([]byte)) {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case msg := <-s.queue:
			handle(msg)
		}
	}
}

// Unsubscribe removes the subscription and waits for its handler to finish.
func (s *inprocSub) Unsubscribe() {
	s.bus.mu.Lock()
	if subs := s.bus.subs[s.topic]; subs != nil {
		delete(subs, s)
		if len(subs) == 0 {
			delete(s.bus.subs, s.topic)
		}
	}
	s.bus.mu.Unlock()
	s.halt()
}

func (s *inprocSub) halt() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
}
