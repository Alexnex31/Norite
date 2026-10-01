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
	"github.com/Alexnex31/Norite/backend/internal/roles"
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
	ChannelID snowflake.ID `json:"channel_id,omitempty"`
	// Need, on a Guild event, is what a recipient must hold beyond view, resolved like view is: in the
	// channel when ChannelID is set. MESSAGE_UPDATE is the case, needing PermReadMessageHistory, because an
	// edit can reach a message that predates the recipient's access and REST refuses them that message.
	Need  roles.Permission `json:"need,omitempty"`
	Users []snowflake.ID   `json:"users,omitempty"`
	// Overwrites, with Snapshot set, are the channel's overwrites as they stood before the change, for an
	// event about a channel whose rows are gone by fan-out time. CHANNEL_DELETE is the case: without them the
	// gateway would have no overwrites to resolve against and would send a hidden channel's deletion to
	// every member, which names a channel they were never shown.
	Overwrites []db.PermissionOverwrite `json:"overwrites,omitempty"`
	// Snapshot says Overwrites is the channel's whole state, even when it is empty, so the gateway resolves
	// against it rather than reading the channel. Its own field because an empty list does not survive
	// encoding as one: without it, a deleted channel that had no overwrites would be read, found missing,
	// and its deletion sent to nobody, which is what a channel event that outlived its channel gets.
	Snapshot bool `json:"snapshot,omitempty"`
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

// RevocationTopic carries revocations: which connections every gateway process must close because the
// sign-in they were opened with has ended. Its own topic rather than an event type on Topic, because a
// revocation is an instruction to the gateway rather than something any client receives, and it must never
// pass through an audience check that could decide nobody is its audience.
const RevocationTopic = "gateway.revocation"

// Revocation names the connections a committed revocation ends: the account's connections opened with a
// sign-in minted before Before, on Device if it is set, or on every device but ExceptDevice if that is set.
//
// Before is what keeps the close from reaching past the revocation. A revocation is published after its
// commit and delivered some time later, and in that time the same device can sign in again and connect; that
// connection's sign-in is newer than the cutoff and is left alone. Snowflakes are ordered by time, which is
// what makes the comparison mean "minted before the revocation" — across replicas only as well as their
// clocks agree, and the gateway's periodic liveness check is what bounds a close that clock skew misses.
type Revocation struct {
	UserID       snowflake.ID `json:"user_id"`
	Before       snowflake.ID `json:"before"`
	Device       string       `json:"device,omitempty"`
	ExceptDevice string       `json:"except_device,omitempty"`
}

// Matches reports whether a connection opened by sessionID, on device, for userID, is one r ends.
func (r Revocation) Matches(userID, sessionID snowflake.ID, device string) bool {
	switch {
	case userID != r.UserID, sessionID >= r.Before:
		return false
	case r.Device != "":
		return device == r.Device
	case r.ExceptDevice != "":
		return device != r.ExceptDevice
	}
	return true
}

// QueueRevocation publishes r once ctx's transaction commits, like Queue: a revocation that rolled back ends
// nothing, and closing before the commit would let a reconnect race the revoke and win.
func (p *Publisher) QueueRevocation(ctx context.Context, r Revocation) error {
	if p == nil || p.bus == nil {
		return nil
	}
	msg, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("dispatch: encoding a revocation: %w", err)
	}
	database.AfterCommit(ctx, func(ctx context.Context) {
		// Logged, not returned, for Queue's reason. A lost revocation is bounded by the gateway's periodic
		// liveness check rather than left open for the life of the connection.
		if err := p.bus.Publish(ctx, RevocationTopic, msg); err != nil {
			p.logger.Error().Err(err).Msg("could not publish a gateway revocation")
		}
	})
	return nil
}
