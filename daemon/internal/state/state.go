// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package state is what the daemon knows from its gateway connection: who is signed in, which guilds they
// are in, and the recent messages of every channel an event has arrived for. In memory only, and lost on
// restart (ADR 0010, tmux semantics); attach clients read it from M20.
//
// # What it holds, and what it deliberately does not
//
// READY carries a summary per guild and nothing bulk (M18): channels, roles and member lists are fetched
// over REST when a guild is opened, which is M20's relay. So the daemon receives CHANNEL_*, ROLES and
// MEMBER events for lists it never loaded, and a list assembled from those events alone would look complete
// and not be. It keeps none of them. What it keeps is what it can keep truthfully:
//
//   - the account, from READY;
//   - the guild summaries, from READY and GUILD_CREATE/UPDATE/DELETE;
//   - a bounded buffer of recent messages per channel, from MESSAGE_CREATE/UPDATE/DELETE.
//
// # When it throws things away
//
// A buffer is only worth showing if it has no holes nobody can see. So:
//
//   - a fresh session (Begin) clears everything: events were missed between the old session and the new one,
//     and history with a silent gap in it is a quiet lie, where an empty buffer refilled over REST is not;
//   - GUILD_PERMISSIONS_UPDATE and GUILD_DELETE clear every message buffer. A channel that drops out of view
//     stops receiving its edits and deletes, so a message a moderator removed would stay here indefinitely.
//     Every buffer and not only the guild's, because a Message names its channel and not its guild, and the
//     daemon holds no channel lists to map one to the other. Both events are rare and happen at human speed.
//
// One staleness is accepted rather than cleared: MESSAGE_UPDATE needs read-history permission and
// MESSAGE_CREATE does not (M18), so a member who can view a channel but not read its history keeps the
// original text of a message edited later. It is what they received live, and REST refuses them the edit too.
//
// # Untrusted text
//
// Names — the account's username and display name, a guild's name — go through termsafe.Text as they
// arrive (rule 19). They are what people identify each other by, so they are what a spoofer forges, and they
// are written to this daemon's log, which is read in a terminal. Message content and a guild's description
// are kept exactly as sent: what is safe to show is the renderer's decision (rule 9's markdown subset at
// M20a), and a sanitizer here would destroy, for every later consumer, the text M20's lossless `--json`
// exists to preserve. Content is never logged.
package state

import (
	"encoding/json"
	"sync"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Limits bound what the state holds. A count per channel bounds nothing in total — the guild caps alone
// allow fifty thousand channels and a message may be 4,000 runes — so there is a byte budget across every
// buffer as well, and the channel that went quiet longest goes first.
type Limits struct {
	// MessagesPerChannel is how many recent messages a channel keeps.
	MessagesPerChannel int
	// TotalBytes is the budget across every channel's buffer, estimated (messageSize).
	TotalBytes int
}

// DefaultLimits are 200 messages a channel and 64 MiB in all.
var DefaultLimits = Limits{MessagesPerChannel: 200, TotalBytes: 64 << 20}

// User is the signed-in account, as READY described it.
type User struct {
	ID          string
	Username    string // sanitized
	DisplayName string // sanitized
}

// Guild is a guild summary.
type Guild struct {
	ID          string
	Name        string // sanitized
	OwnerID     string
	Description *string // as sent
}

// Message is a message as the gateway sent it. Content is as sent and unsanitized: see the package comment.
type Message = apicontract.Message

// State is the daemon's view. It is a gatewayclient.Sink, and safe to read from any goroutine.
type State struct {
	log    zerolog.Logger
	limits Limits

	mu         sync.RWMutex
	generation uint64
	user       *User
	guilds     map[string]Guild
	channels   map[string]*buffer
	bytes      int
	tick       uint64 // orders channel activity for eviction
}

type buffer struct {
	messages []Message
	bytes    int
	active   uint64
}

// New builds an empty State. Zero limits mean DefaultLimits.
func New(log zerolog.Logger, limits Limits) *State {
	if limits.MessagesPerChannel <= 0 {
		limits.MessagesPerChannel = DefaultLimits.MessagesPerChannel
	}
	if limits.TotalBytes <= 0 {
		limits.TotalBytes = DefaultLimits.TotalBytes
	}
	return &State{
		log: log, limits: limits,
		guilds: map[string]Guild{}, channels: map[string]*buffer{},
	}
}

// Begin forgets everything: a fresh session is starting, and whatever was built from the last one may have
// missed events.
func (s *State) Begin(generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation = generation
	s.user = nil
	s.guilds = map[string]Guild{}
	s.dropMessagesLocked()
}

// Dispatch applies one event.
func (s *State) Dispatch(eventType string, data json.RawMessage) {
	var err error
	switch eventType {
	case "READY":
		err = s.ready(data)
	case "GUILD_CREATE", "GUILD_UPDATE":
		var g apicontract.Guild
		if err = json.Unmarshal(data, &g); err == nil {
			s.mu.Lock()
			s.guilds[g.Id] = guildOf(g)
			s.mu.Unlock()
		}
	case "GUILD_DELETE":
		var gone struct {
			ID string `json:"id"`
		}
		if err = json.Unmarshal(data, &gone); err == nil {
			s.mu.Lock()
			delete(s.guilds, gone.ID)
			s.dropMessagesLocked()
			s.mu.Unlock()
		}
	case "GUILD_PERMISSIONS_UPDATE":
		s.mu.Lock()
		s.dropMessagesLocked()
		s.mu.Unlock()
	case "MESSAGE_CREATE":
		var m Message
		if err = json.Unmarshal(data, &m); err == nil {
			s.mu.Lock()
			s.addLocked(m)
			s.mu.Unlock()
		}
	case "MESSAGE_UPDATE":
		var m Message
		if err = json.Unmarshal(data, &m); err == nil {
			s.mu.Lock()
			s.replaceLocked(m)
			s.mu.Unlock()
		}
	case "MESSAGE_DELETE":
		// The gateway schema's MessageDeleted, which has no REST counterpart and so nothing generated.
		var gone struct {
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
		}
		if err = json.Unmarshal(data, &gone); err == nil {
			s.mu.Lock()
			s.removeLocked(gone.ChannelID, gone.ID)
			s.mu.Unlock()
		}
	default:
		// RESUMED, and the channel, role and member events whose lists this state does not hold — see the
		// package comment. Attach clients receive them from M20 regardless.
		return
	}
	if err != nil {
		// The type and nothing of the payload, which may be a message.
		s.log.Error().Str("type", termsafe.Text(eventType)).Msg("a gateway event could not be decoded; ignoring it")
	}
}

func (s *State) ready(data json.RawMessage) error {
	var r struct {
		User   apicontract.User    `json:"user"`
		Guilds []apicontract.Guild `json:"guilds"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	user := User{
		ID:          r.User.Id,
		Username:    termsafe.Text(r.User.Username),
		DisplayName: termsafe.Text(r.User.DisplayName),
	}
	guilds := make(map[string]Guild, len(r.Guilds))
	for _, g := range r.Guilds {
		guilds[g.Id] = guildOf(g)
	}

	s.mu.Lock()
	s.user = &user
	s.guilds = guilds
	s.mu.Unlock()

	// Names a stranger's instance chose, sanitized on the way in rather than relying on `norite login` to
	// have done it — the case M7 deferred to here.
	s.log.Info().Str("username", user.Username).Str("display_name", user.DisplayName).
		Int("guilds", len(guilds)).Msg("ready")
	return nil
}

func guildOf(g apicontract.Guild) Guild {
	return Guild{ID: g.Id, Name: termsafe.Text(g.Name), OwnerID: g.OwnerId, Description: g.Description}
}

// messageSize estimates what a message costs to hold: its content and a fixed allowance for the rest. An
// estimate on purpose — the budget bounds growth, it does not account for every byte.
func messageSize(m Message) int { return len(m.Content) + 256 }

func (s *State) addLocked(m Message) {
	b := s.channels[m.ChannelId]
	if b == nil {
		b = &buffer{}
		s.channels[m.ChannelId] = b
	}
	for _, have := range b.messages {
		if have.Id == m.Id {
			return // a replay after RESUME can repeat what was already applied
		}
	}
	s.tick++
	b.active = s.tick
	b.messages = append(b.messages, m)
	size := messageSize(m)
	b.bytes += size
	s.bytes += size

	for len(b.messages) > s.limits.MessagesPerChannel {
		s.trimOldestLocked(b)
	}
	s.enforceBudgetLocked(m.ChannelId)
}

func (s *State) trimOldestLocked(b *buffer) {
	size := messageSize(b.messages[0])
	b.messages[0] = Message{}
	b.messages = b.messages[1:]
	b.bytes -= size
	s.bytes -= size
}

// enforceBudgetLocked evicts whole channels, quietest first, until the budget holds. The channel just written
// to is evicted last: it is the one somebody is most likely looking at.
func (s *State) enforceBudgetLocked(keep string) {
	for s.bytes > s.limits.TotalBytes {
		victim, oldest := "", uint64(0)
		for id, b := range s.channels {
			if id == keep {
				continue
			}
			if victim == "" || b.active < oldest {
				victim, oldest = id, b.active
			}
		}
		if victim == "" {
			// Only the channel just written to is left, and it alone is over budget: trim it instead.
			b := s.channels[keep]
			if len(b.messages) <= 1 {
				return
			}
			s.trimOldestLocked(b)
			continue
		}
		s.bytes -= s.channels[victim].bytes
		delete(s.channels, victim)
	}
}

func (s *State) replaceLocked(m Message) {
	b := s.channels[m.ChannelId]
	if b == nil {
		return
	}
	for i := range b.messages {
		if b.messages[i].Id == m.Id {
			delta := messageSize(m) - messageSize(b.messages[i])
			b.messages[i] = m
			b.bytes += delta
			s.bytes += delta
			s.enforceBudgetLocked(m.ChannelId)
			return
		}
	}
}

func (s *State) removeLocked(channelID, messageID string) {
	b := s.channels[channelID]
	if b == nil {
		return
	}
	for i := range b.messages {
		if b.messages[i].Id == messageID {
			size := messageSize(b.messages[i])
			b.messages = append(b.messages[:i], b.messages[i+1:]...)
			b.bytes -= size
			s.bytes -= size
			return
		}
	}
}

func (s *State) dropMessagesLocked() {
	s.channels = map[string]*buffer{}
	s.bytes = 0
}

// ---------- reading ----------

// User returns the signed-in account, or false before READY.
func (s *State) User() (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.user == nil {
		return User{}, false
	}
	return *s.user, true
}

// Guilds returns the guild summaries, in no particular order.
func (s *State) Guilds() []Guild {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Guild, 0, len(s.guilds))
	for _, g := range s.guilds {
		out = append(out, g)
	}
	return out
}

// Messages returns a channel's buffered messages, oldest first: what arrived since the session began, at
// most Limits.MessagesPerChannel of it, and nothing from before.
func (s *State) Messages(channelID string) []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b := s.channels[channelID]
	if b == nil {
		return nil
	}
	return append([]Message(nil), b.messages...)
}

// Bytes reports the estimated size of every buffer together.
func (s *State) Bytes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bytes
}

// Generation reports which sign-in the state was built from.
func (s *State) Generation() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation
}
