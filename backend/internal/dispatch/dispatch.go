// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dispatch is how a committed change becomes a gateway event: the publishing half, which the domain
// packages call, and the event shape the gateway consumes.
//
// It exists as its own package so neither side imports the other. guilds and messages cannot import the
// gateway, which imports them for READY; the gateway cannot be the place events are described, or every
// publisher would depend on the connection code. So the event is described here, once.
//
// Two rules are structural rather than remembered:
//
//   - **Rule 5.** Queue takes the transaction's ctx and registers the publish with database.AfterCommit, so
//     an event is published only once its change is durable and never if it rolls back. There is no other
//     way to publish, so there is no way to publish early.
//   - **Rule 1.** An event names an audience, not a list of who may see it. Who actually receives it is
//     decided by the gateway at fan-out time, against fresh rows: membership for a guild event, and view
//     permission on the channel for a channel event. A publisher cannot widen who sees something by getting
//     its recipient list wrong, because it does not write one.
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/events"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Topic is the bus topic every gateway event travels on. One topic rather than one per guild, so a process
// needs one subscription however many guilds its connections are in, and so one publisher's events reach a
// subscriber in the order they were published (events.Bus orders per subscription).
const Topic = "gateway.dispatch"

// Audience says who an event is for, before the gateway checks who may see it.
type Audience string

const (
	// Guild is every current member of GuildID; with ChannelID set, only those who can view that channel.
	Guild Audience = "guild"
	// Users is exactly Users. For the events about an account's own membership, where the account is the
	// audience and the guild's other members are not: the guild a member created, the guild a member was
	// removed from.
	Users Audience = "users"
	// FormerMembers is every connection that still has GuildID in its READY set, with no membership check.
	// Only for GUILD_DELETE of a guild that no longer exists, where there are no rows left to check against
	// and the only thing disclosed is the id of a guild the connection was already told about.
	FormerMembers Audience = "former_members"
)

// Event is one gateway dispatch, as it travels on the bus.
type Event struct {
	// Type is the dispatch's t, e.g. "MESSAGE_CREATE".
	Type     string       `json:"type"`
	Audience Audience     `json:"audience"`
	GuildID  snowflake.ID `json:"guild_id"`
	// ChannelID, when set on a Guild event, narrows it to members who can view that channel.
	ChannelID snowflake.ID   `json:"channel_id,omitempty"`
	Users     []snowflake.ID `json:"users,omitempty"`
	// Overwrites, when set, are the channel's overwrites as they stood before the change, for an event
	// about a channel whose rows are gone by fan-out time. CHANNEL_DELETE is the case: without them the
	// gateway would have no overwrites to resolve against and would send a hidden channel's deletion to
	// every member, which names a channel they were never shown.
	Overwrites []db.PermissionOverwrite `json:"overwrites,omitempty"`
	// Data is the dispatch's d, already encoded: the same bytes go to every recipient.
	Data json.RawMessage `json:"data"`
}

// Publisher queues events for publication after commit.
//
// The zero value and a nil *Publisher both drop events. That is what the domain packages' own tests get,
// which exercise a service without a gateway; cmd/server always passes a real one, and the gateway's tests
// are what prove it arrives.
type Publisher struct {
	bus    events.Bus
	logger *zerolog.Logger
}

// NewPublisher returns a Publisher over bus.
func NewPublisher(bus events.Bus, logger *zerolog.Logger) *Publisher {
	if logger == nil {
		nop := zerolog.Nop()
		logger = &nop
	}
	return &Publisher{bus: bus, logger: logger}
}

// Queue encodes data now and publishes the event once ctx's transaction commits.
//
// Encoded now rather than after commit, so a payload that cannot be encoded fails the request that made it
// while its transaction can still roll back, rather than vanishing after the change is durable. ctx must be
// the transaction's (see database.RunInTx); anything else panics in AfterCommit, which is the point.
func (p *Publisher) Queue(ctx context.Context, ev Event, data any) error {
	if p == nil || p.bus == nil {
		return nil
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("dispatch: encoding %s: %w", ev.Type, err)
	}
	ev.Data = encoded
	msg, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("dispatch: encoding the %s envelope: %w", ev.Type, err)
	}
	database.AfterCommit(ctx, func(ctx context.Context) {
		// A failed publish is logged, not returned: the change has committed, and telling the caller it
		// failed would be false. The connections that missed it resync on their next READY; the bus is
		// at-most-once by design (events package comment).
		if err := p.bus.Publish(ctx, Topic, msg); err != nil {
			p.logger.Error().Err(err).Str("type", ev.Type).Msg("could not publish a gateway event")
		}
	})
	return nil
}
