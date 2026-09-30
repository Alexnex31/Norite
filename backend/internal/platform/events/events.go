// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package events is the backend's publish/subscribe bus: how something that happened in one request reaches
// the code that has to react to it — the gateway fanning an event out to connections, a revocation closing
// the connections it ended.
//
// Two implementations, chosen by configuration (docs/architecture.md §2, §15.7). InProc is the default and
// what every self-hosted single-process instance runs. Redis carries the same traffic between processes, and
// is what lets the flagship run more than one replica (M114); nothing activates it before then except the
// tests, which run every property below against both.
//
// # What the bus promises, and what it does not
//
//   - **Bytes, never values.** A payload is serialized before Publish and decoded by the subscriber, in both
//     implementations. InProc could pass a Go value, and would then hide every event that does not survive
//     a round trip through Redis until the flagship ran it.
//   - **At most once.** A subscriber that falls behind has messages dropped rather than slowing the
//     publisher, which is what Redis pub/sub does and what InProc does to match. Publishers are after-commit
//     hooks on a request's path; a bus that could block them would make one slow subscriber every request's
//     latency. Anything that must not be lost cannot rely on the bus alone, and says how it recovers — the
//     gateway re-checks a connection's session periodically, so a lost close is bounded rather than
//     permanent.
//   - **Ordered per subscription**, for one publisher's messages. Across publishers there is no order to
//     preserve: two requests committing concurrently have none.
//   - **Published after commit.** The bus does not know about transactions; callers publish from
//     database.AfterCommit, which is what makes rule 5 hold by construction.
package events

import (
	"context"
	"errors"
)

// Bus publishes payloads to topics and delivers them to that topic's subscribers.
type Bus interface {
	// Publish sends payload to every current subscriber of topic. It returns once the payload has been
	// handed to the transport, not once anybody has handled it.
	Publish(ctx context.Context, topic string, payload []byte) error

	// Subscribe calls handle with each payload published to topic from the moment Subscribe returns, one
	// at a time and in order, on a goroutine of the bus's own. A handler that blocks delays only its own
	// subscription, and past the buffer causes that subscription's messages to be dropped.
	//
	// handle must not keep payload after it returns: the slice may be reused.
	Subscribe(topic string, handle func(payload []byte)) (Subscription, error)

	// Close stops every subscription and releases the transport. Publishing after Close is an error.
	Close() error
}

// Subscription is one registered handler.
type Subscription interface {
	// Unsubscribe stops delivery and waits for a handler call in progress to return, so the caller may tear
	// down whatever the handler touches as soon as it returns. Because it waits for the handler, calling it
	// from inside that handler deadlocks.
	Unsubscribe()
}

// ErrClosed is returned by a bus used after Close.
var ErrClosed = errors.New("events: bus is closed")

// subscriptionBuffer is how many payloads a subscription holds before dropping.
//
// Sized for bursts, not for a subscriber that is simply too slow: a guild-wide event fans out to one topic
// and one handler per process, so a few thousand queued means the handler is falling behind, and a larger
// buffer would only make it fall further behind before anybody noticed.
const subscriptionBuffer = 4096
