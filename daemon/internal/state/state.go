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
// are written to this daemon's log, which is read in a terminal. Message content is kept exactly as sent: what is safe to show is the renderer's decision (rule 9's markdown subset at
// M20a), and a sanitizer here would destroy, for every later consumer, the text M20's lossless `--json`
// exists to preserve. Content is never logged.
package state

import (
	"container/list"
	"encoding/json"
	"sync"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Limits bound what the state holds. A count per channel bounds nothing in total — the guild caps alone
// allow fifty thousand channels and a message may be 4,000 runes — so there is a byte budget across every
// buffer as well, and the channel that went quiet longest goes first.
//
// Bounded against the instance, not only against an instance behaving well. The instance is a stranger's
// server (rule 19 treats its text that way), and every limit here holds whatever it sends: a message is
// charged its whole payload rather than its content, and guild summaries are capped in number and length,
// where the real server's own validation would have bounded them for it (M19 /security-sweep).
type Limits struct {
	// MessagesPerChannel is how many recent messages a channel keeps.
	MessagesPerChannel int
	// TotalBytes is the budget across every channel's buffer, charged per message as its payload's length.
	TotalBytes int
}

// DefaultLimits are 200 messages a channel and 64 MiB in all.
var DefaultLimits = Limits{MessagesPerChannel: 200, TotalBytes: 64 << 20}

const (
	// maxGuilds is ten times the joined-guild cap the server enforces (M72a's 100): room for that cap to
	// grow, and none for an instance announcing guilds without limit.
	maxGuilds = 1000
	// maxNameRunes is the server's own limit on a guild name. A longer one did not come from a server
	// validating its input, and is cut rather than stored at whatever length arrived.
	maxNameRunes = 100
	// entryOverhead is charged on top of each payload for the bookkeeping that holds it.
	entryOverhead = 64
)

// User is the signed-in account, as READY described it.
type User struct {
	ID          string
	Username    string // sanitized
	DisplayName string // sanitized
}

// Guild is a guild summary: what READY carries, and no more. A guild's description is not kept — nothing a
// summary is for needs it, and it is the one free-text field a guild has.
type Guild struct {
	ID      string
	Name    string // sanitized, at most maxNameRunes
	OwnerID string
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
	// recency orders channels by activity, most recent at the front, so evicting the quietest is O(1)
	// whatever the number of channels — which an instance, not the daemon, decides.
	recency *list.List
	bytes   int
}

type buffer struct {
	entries []entry
	bytes   int
	place   *list.Element // in recency; its value is the channel id
}

type entry struct {
	message Message
	size    int
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
		guilds: map[string]Guild{}, channels: map[string]*buffer{}, recency: list.New(),
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
			s.putGuildLocked(g)
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
			s.addLocked(entry{message: m, size: len(data) + entryOverhead})
			s.mu.Unlock()
		}
	case "MESSAGE_UPDATE":
		var m Message
		if err = json.Unmarshal(data, &m); err == nil {
			s.mu.Lock()
			s.replaceLocked(entry{message: m, size: len(data) + entryOverhead})
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

	s.mu.Lock()
	s.user = &user
	s.guilds = map[string]Guild{}
	for _, g := range r.Guilds {
		s.putGuildLocked(g)
	}
	count := len(s.guilds)
	s.mu.Unlock()

	// Names a stranger's instance chose, sanitized on the way in rather than relying on `norite login` to
	// have done it — the case M7 deferred to here.
	s.log.Info().Str("username", user.Username).Str("display_name", user.DisplayName).
		Int("guilds", count).Msg("ready")
	return nil
}

// putGuildLocked stores a guild summary, refusing a new one past maxGuilds.
func (s *State) putGuildLocked(g apicontract.Guild) {
	if _, known := s.guilds[g.Id]; !known && len(s.guilds) >= maxGuilds {
		s.log.Warn().Int("limit", maxGuilds).Msg("the instance announced more guilds than this daemon keeps; " +
			"ignoring the rest")
		return
	}
	s.guilds[g.Id] = Guild{ID: g.Id, Name: truncateRunes(termsafe.Text(g.Name), maxNameRunes), OwnerID: g.OwnerId}
}

func truncateRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

func (s *State) addLocked(e entry) {
	id := e.message.ChannelId
	b := s.channels[id]
	if b == nil {
		b = &buffer{place: s.recency.PushFront(id)}
		s.channels[id] = b
	}
	for _, have := range b.entries {
		if have.message.Id == e.message.Id {
			return // a replay after RESUME can repeat what was already applied
		}
	}
	s.recency.MoveToFront(b.place)
	b.entries = append(b.entries, e)
	b.bytes += e.size
	s.bytes += e.size

	for len(b.entries) > s.limits.MessagesPerChannel {
		s.trimOldestLocked(b)
	}
	s.enforceBudgetLocked(id)
}

func (s *State) trimOldestLocked(b *buffer) {
	size := b.entries[0].size
	b.entries[0] = entry{}
	b.entries = b.entries[1:]
	b.bytes -= size
	s.bytes -= size
}

// enforceBudgetLocked evicts whole channels, quietest first, until the budget holds. The channel just written
// to is evicted last: it is the one somebody is most likely looking at.
func (s *State) enforceBudgetLocked(keep string) {
	for s.bytes > s.limits.TotalBytes {
		back := s.recency.Back()
		if victim := back.Value.(string); victim != keep { //nolint:forcetypeassert // only strings are pushed
			s.bytes -= s.channels[victim].bytes
			s.recency.Remove(back)
			delete(s.channels, victim)
			continue
		}
		// Only the channel just written to is left, and it alone is over budget: trim it instead.
		b := s.channels[keep]
		if len(b.entries) <= 1 {
			return
		}
		s.trimOldestLocked(b)
	}
}

func (s *State) replaceLocked(e entry) {
	b := s.channels[e.message.ChannelId]
	if b == nil {
		return
	}
	for i := range b.entries {
		if b.entries[i].message.Id == e.message.Id {
			delta := e.size - b.entries[i].size
			b.entries[i] = e
			b.bytes += delta
			s.bytes += delta
			s.enforceBudgetLocked(e.message.ChannelId)
			return
		}
	}
}

func (s *State) removeLocked(channelID, messageID string) {
	b := s.channels[channelID]
	if b == nil {
		return
	}
	for i := range b.entries {
		if b.entries[i].message.Id == messageID {
			size := b.entries[i].size
			b.entries = append(b.entries[:i], b.entries[i+1:]...)
			b.bytes -= size
			s.bytes -= size
			return
		}
	}
}

func (s *State) dropMessagesLocked() {
	s.channels = map[string]*buffer{}
	s.recency.Init()
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
	out := make([]Message, len(b.entries))
	for i, e := range b.entries {
		out[i] = e.message
	}
	return out
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
