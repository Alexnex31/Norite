// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package state

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The payloads below are the contract's shapes. The gateway client's tests validate every frame against
// gateway-events.schema.json; these feed the state directly, so they are kept to the same fields.

func readyPayload(username, displayName string, guilds ...map[string]any) json.RawMessage {
	if guilds == nil {
		guilds = []map[string]any{}
	}
	return mustJSON(map[string]any{
		"session_id": "sess-1",
		"user": map[string]any{
			"id": "1", "username": username, "display_name": displayName, "email": "ada@example.com",
			"created_at": "2026-01-01T00:00:00Z",
		},
		"guilds": guilds,
	})
}

func guild(id, name string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "owner_id": "1", "icon_hash": nil, "description": nil,
		"message_audit_enabled": false, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
	}
}

func message(id, channelID, content string) json.RawMessage {
	return mustJSON(map[string]any{
		"id": id, "channel_id": channelID, "author_id": "1", "content": content, "type": 0,
		"reply_to_id": nil, "edited_at": nil, "created_at": "2026-01-01T00:00:00Z", "tags": nil,
	})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

type logBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func newState(limits Limits) (*State, *logBuffer) {
	logs := &logBuffer{}
	return New(zerolog.New(logs).Level(zerolog.DebugLevel), limits), logs
}

func contents(ms []Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Content
	}
	return out
}

// terminalActive reports whether s holds anything termsafe exists to remove: a control character, or a
// bidi embedding, override or isolate.
func terminalActive(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return true
		}
	}
	return false
}

// ---------- the done-when: names sanitized by the daemon ----------

// Until M19 every name the daemon logged had been sanitized by the `norite login` that stored it. Here the
// daemon receives names of its own, from an instance it does not control, and sanitizes them itself.
func TestNamesAStrangersInstanceChoseAreSanitizedOnArrival(t *testing.T) {
	s, logs := newState(Limits{})
	hostile := "\x1b]0;pwned\x07Ada\x1b[2J\u202enimda"
	s.Begin(1)
	s.Dispatch("READY", readyPayload(hostile, hostile, guild("10", hostile)))
	s.Dispatch("GUILD_CREATE", mustJSON(guild("11", hostile)))

	u, ok := s.User()
	require.True(t, ok)
	for what, v := range map[string]string{"username": u.Username, "display name": u.DisplayName} {
		assert.False(t, terminalActive(v), "the %s %q reached the state unsanitized", what, v)
		assert.Contains(t, v, "Ada", "sanitizing removes what acts on a terminal and keeps the rest")
	}
	for _, g := range s.Guilds() {
		assert.False(t, terminalActive(g.Name), "guild %s's name %q reached the state unsanitized", g.ID, g.Name)
	}

	// And the log, which is the surface M7's deferral was about: it is read with `cat`.
	assert.Contains(t, logs.String(), `"ready"`)
	// zerolog ends each line with a newline of its own; what a name brought with it is the question.
	assert.False(t, terminalActive(strings.ReplaceAll(logs.String(), "\n", "")),
		"the log carries a terminal-active character")
	assert.NotContains(t, logs.String(), `\u001b`, "an escape must be removed, not merely escaped by zerolog")
	assert.NotContains(t, logs.String(), "\u202e")
}

// Content is the renderer's to make safe (rule 9's markdown subset, M20a), and a sanitizer here would destroy
// for every later consumer the text M20's lossless --json exists to preserve. Kept as sent — and never logged.
func TestMessageContentIsKeptAsSentAndNeverLogged(t *testing.T) {
	s, logs := newState(Limits{})
	s.Begin(1)
	raw := "line one\nline two \x1b[31mred\x1b[0m"
	s.Dispatch("MESSAGE_CREATE", message("20", "30", raw))

	assert.Equal(t, []string{raw}, contents(s.Messages("30")))
	assert.NotContains(t, logs.String(), "line one")
}

// ---------- building state ----------

func TestReadyAndGuildEventsMaintainTheGuildSummaries(t *testing.T) {
	s, _ := newState(Limits{})
	s.Begin(1)
	s.Dispatch("READY", readyPayload("ada", "Ada", guild("10", "Ten"), guild("11", "Eleven")))
	s.Dispatch("GUILD_CREATE", mustJSON(guild("12", "Twelve")))
	s.Dispatch("GUILD_UPDATE", mustJSON(guild("10", "Ten, renamed")))
	s.Dispatch("GUILD_DELETE", mustJSON(map[string]any{"id": "11"}))

	names := map[string]string{}
	for _, g := range s.Guilds() {
		names[g.ID] = g.Name
	}
	assert.Equal(t, map[string]string{"10": "Ten, renamed", "12": "Twelve"}, names)
	u, ok := s.User()
	require.True(t, ok)
	assert.Equal(t, "ada", u.Username)
	assert.Equal(t, uint64(1), s.Generation())
}

func TestMessagesAreBufferedPerChannelAndFollowEditsAndDeletes(t *testing.T) {
	s, _ := newState(Limits{})
	s.Begin(1)
	s.Dispatch("MESSAGE_CREATE", message("1", "30", "first"))
	s.Dispatch("MESSAGE_CREATE", message("2", "30", "second"))
	s.Dispatch("MESSAGE_CREATE", message("3", "31", "elsewhere"))
	s.Dispatch("MESSAGE_UPDATE", message("1", "30", "first, edited"))
	s.Dispatch("MESSAGE_DELETE", mustJSON(map[string]any{"id": "2", "channel_id": "30", "guild_id": "10"}))

	assert.Equal(t, []string{"first, edited"}, contents(s.Messages("30")))
	assert.Equal(t, []string{"elsewhere"}, contents(s.Messages("31")))
	assert.Nil(t, s.Messages("32"), "a channel nothing arrived for holds nothing, rather than an empty history")
}

// RESUME replays every frame after the client's last sequence number, and a client that applied a frame and
// then lost the connection before recording its number sees that frame again.
func TestAReplayedMessageIsNotDuplicated(t *testing.T) {
	s, _ := newState(Limits{})
	s.Begin(1)
	s.Dispatch("MESSAGE_CREATE", message("1", "30", "once"))
	s.Dispatch("MESSAGE_CREATE", message("1", "30", "once"))
	assert.Equal(t, []string{"once"}, contents(s.Messages("30")))
}

// An edit to a message this daemon never received — sent before the session began — is not invented into a
// buffer that would then look as though it held that channel's history.
func TestAnEditToAnUnseenMessageIsNotApplied(t *testing.T) {
	s, _ := newState(Limits{})
	s.Begin(1)
	s.Dispatch("MESSAGE_UPDATE", message("1", "30", "edited before we arrived"))
	assert.Nil(t, s.Messages("30"))
}

// ---------- throwing things away ----------

// A fresh session means events were missed. History with a silent gap is a quiet lie.
func TestAFreshSessionForgetsEverything(t *testing.T) {
	s, _ := newState(Limits{})
	s.Begin(1)
	s.Dispatch("READY", readyPayload("ada", "Ada", guild("10", "Ten")))
	s.Dispatch("MESSAGE_CREATE", message("1", "30", "before"))

	s.Begin(1)
	_, ok := s.User()
	assert.False(t, ok)
	assert.Empty(t, s.Guilds())
	assert.Nil(t, s.Messages("30"))
	assert.Zero(t, s.Bytes())
}

// A channel that drops out of view stops receiving its edits and deletes, so a message a moderator removes
// afterwards would stay in the daemon's memory indefinitely. Every buffer goes, because a Message names its
// channel and not its guild.
func TestAPermissionChangeOrARemovalClearsTheBuffers(t *testing.T) {
	for _, event := range []struct {
		t string
		d json.RawMessage
	}{
		{"GUILD_PERMISSIONS_UPDATE", mustJSON(map[string]any{"guild_id": "10"})},
		{"GUILD_DELETE", mustJSON(map[string]any{"id": "10"})},
	} {
		t.Run(event.t, func(t *testing.T) {
			s, _ := newState(Limits{})
			s.Begin(1)
			s.Dispatch("READY", readyPayload("ada", "Ada", guild("10", "Ten"), guild("11", "Eleven")))
			s.Dispatch("MESSAGE_CREATE", message("1", "30", "in ten"))
			s.Dispatch("MESSAGE_CREATE", message("2", "31", "in eleven"))

			s.Dispatch(event.t, event.d)
			assert.Nil(t, s.Messages("30"))
			assert.Nil(t, s.Messages("31"))
			assert.Zero(t, s.Bytes())
			assert.NotEmpty(t, s.Guilds(), "the summaries are not what went stale")
		})
	}
}

func TestEachChannelKeepsItsMostRecentMessages(t *testing.T) {
	s, _ := newState(Limits{MessagesPerChannel: 3})
	s.Begin(1)
	for i := range 5 {
		s.Dispatch("MESSAGE_CREATE", message(fmt.Sprint(i), "30", fmt.Sprint("m", i)))
	}
	assert.Equal(t, []string{"m2", "m3", "m4"}, contents(s.Messages("30")))
	assert.Equal(t, 3*charge(message("2", "30", "m2")), s.Bytes())
}

// The guild caps allow fifty thousand channels, so a count per channel bounds nothing in total. The budget
// does, and the channel that went quiet longest goes first.
// charge is what holding a message costs against the budget: its whole payload, and the bookkeeping.
func charge(payload json.RawMessage) int { return len(payload) + entryOverhead }

func TestTheByteBudgetEvictsTheQuietestChannel(t *testing.T) {
	one := charge(message("1", "a", "0123456789"))
	s, _ := newState(Limits{MessagesPerChannel: 100, TotalBytes: 3 * one})
	s.Begin(1)
	s.Dispatch("MESSAGE_CREATE", message("1", "a", "0123456789"))
	s.Dispatch("MESSAGE_CREATE", message("2", "b", "0123456789"))
	s.Dispatch("MESSAGE_CREATE", message("3", "c", "0123456789"))
	s.Dispatch("MESSAGE_CREATE", message("4", "a", "0123456789")) // a is active again
	assert.LessOrEqual(t, s.Bytes(), 3*one)
	assert.Nil(t, s.Messages("b"), "the quietest channel is evicted")
	assert.Len(t, s.Messages("a"), 2)
	assert.Len(t, s.Messages("c"), 1)
}

func TestAChannelAloneOverBudgetIsTrimmedRatherThanLetGrow(t *testing.T) {
	one := charge(message("1", "a", "0123456789"))
	s, _ := newState(Limits{MessagesPerChannel: 100, TotalBytes: 2 * one})
	s.Begin(1)
	for i := range 5 {
		s.Dispatch("MESSAGE_CREATE", message(fmt.Sprint(i), "a", "0123456789"))
	}
	assert.LessOrEqual(t, s.Bytes(), 2*one)
	assert.Len(t, s.Messages("a"), 2)
}

// The instance is a stranger's server. A message is charged everything it arrived with, so padding a field
// other than content — tags, here — cannot hold memory the budget does not count (M19 /security-sweep).
func TestAMessageIsChargedItsWholePayload(t *testing.T) {
	tags := make([]map[string]any, 2000)
	for i := range tags {
		tags[i] = map[string]any{"id": fmt.Sprint(i), "name": strings.Repeat("x", 100), "is_shared": true}
	}
	padded := mustJSON(map[string]any{
		"id": "1", "channel_id": "30", "author_id": "1", "content": "hi", "type": 0,
		"reply_to_id": nil, "edited_at": nil, "created_at": "2026-01-01T00:00:00Z", "tags": tags,
	})
	s, _ := newState(Limits{})
	s.Begin(1)
	s.Dispatch("MESSAGE_CREATE", padded)
	assert.GreaterOrEqual(t, s.Bytes(), len(padded))
}

// A real server caps a member at a hundred guilds and a name at a hundred runes. This daemon does not rely
// on the instance having validated anything.
func TestGuildSummariesAreBoundedInNumberAndLength(t *testing.T) {
	s, logs := newState(Limits{})
	s.Begin(1)
	for i := range maxGuilds + 100 {
		s.Dispatch("GUILD_CREATE", mustJSON(guild(fmt.Sprint(i), strings.Repeat("é", 500))))
	}
	gs := s.Guilds()
	assert.Len(t, gs, maxGuilds)
	for _, g := range gs {
		require.Equal(t, maxNameRunes, len([]rune(g.Name)))
	}
	assert.Contains(t, logs.String(), "more guilds than this daemon keeps")

	// A guild already held is still updated at the cap.
	s.Dispatch("GUILD_UPDATE", mustJSON(guild("0", "renamed")))
	for _, g := range s.Guilds() {
		if g.ID == "0" {
			assert.Equal(t, "renamed", g.Name)
		}
	}
}

// ---------- what it survives ----------

// The type and nothing of the payload: a payload may be a message.
func TestAnUndecodableEventIsIgnoredAndLogsNoPayload(t *testing.T) {
	s, logs := newState(Limits{})
	s.Begin(1)
	s.Dispatch("MESSAGE_CREATE", json.RawMessage(`{"content": "secret words", "id": 7}`))
	assert.Contains(t, logs.String(), "could not be decoded")
	assert.NotContains(t, logs.String(), "secret words")
}

func TestReadsAndWritesMayRace(t *testing.T) {
	s, _ := newState(Limits{MessagesPerChannel: 10})
	s.Begin(1)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 500 {
			s.Dispatch("MESSAGE_CREATE", message(fmt.Sprint(i), fmt.Sprint(i%7), "x"))
			if i%50 == 0 {
				s.Dispatch("GUILD_PERMISSIONS_UPDATE", mustJSON(map[string]any{"guild_id": "1"}))
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			for i := range 500 {
				_ = s.Messages(fmt.Sprint(i % 7))
				_ = s.Guilds()
				_ = s.Bytes()
			}
		})
	}
	wg.Wait()
}
