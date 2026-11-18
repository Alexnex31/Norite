// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package coordinator is the backend's half of a voice call (M33): what op 4 asks of it, what ends a call,
// and what the channel is told. It authorizes a join against fresh rows, mints the connection, provisions it
// on the SFU through voice.Control, records it in voice_states and answers with the SFU's answer and a TURN
// credential. docs/architecture.md §6 calls it the MediaCoordinator.
//
// It lives beside the SFU and not inside it because it reads the database and the SFU must not: on the
// flagship the SFU runs in pods kept away from Postgres (ADR 0023), and voice.Control is all that crosses.
//
// # What ends a call
//
//   - **Its session.** A call belongs to the gateway session that joined it (M33's D3). The gateway calls
//     Ended when it drops a session, by a revocation or by its resume window running out, so the end of a
//     sign-in ends the call with nothing a sixth revocation path has to remember.
//   - **A newer join.** A session holds one call, and a join replaces the call of any older session of the
//     same device: a restarted daemon's join is not refused by the call its previous run left behind.
//   - **A lost right.** guilds publishes a re-check on dispatch.VoiceTopic from every mutation that can take
//     PermConnectVoice away (D4). Each process re-resolves the calls it holds in that guild, joins under way
//     included, which register before they are authorized, and evicts with a cutoff that refuses a
//     provision issued before it.
//   - **Leaving**, op 4 with no channel.
package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/events"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/ratelimit"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
	"github.com/Alexnex31/Norite/backend/internal/voice"
	"github.com/Alexnex31/Norite/backend/internal/voice/turn"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicestate"
)

// ChannelGuildVoice is channels.type for a voice channel. guilds.ChannelGuildVoice is the same value, and
// cmd/server's TestTheVoiceChannelTypeAgreesAcrossPackages holds the two equal: coordinator does not import
// guilds, which would make guilds' callers link the SFU.
const ChannelGuildVoice int16 = 2

// The channel's bitrate as a caller is told it (M33's D9): 128 kbps when the channel names none, and
// clamped to what the contract accepts, since a row written before M33 may hold 8 or 384 kbps. The ceiling
// is the most a 20 ms Opus packet with its RTP headers carries under the SFU's 256 kbps per-track bound,
// measured on speech at 239 kbit/s in its worst second.
const (
	DefaultBitrate = 128_000
	MinBitrate     = 16_000
	MaxBitrate     = 224_000
)

// joinRate bounds joins per account, across replicas (D8). Ten a minute is a person switching channels
// briskly; each join costs a provision the SFU holds for up to its timeout.
const joinRate = "10-M"

// beforeProvision is a test's hook between a join's authorization and its provision, where a re-check
// racing the join is staged rather than raced (CLAUDE.md, "a race is staged").
var beforeProvision func()

// beforeReplace is the same between authorization and the replacing of the device's older calls, where a
// newer join of the same device is staged.
var beforeReplace func()

// endWithin bounds an end the gateway asks for from a goroutine of its own.
const endWithin = 10 * time.Second

// Options configures a Coordinator.
type Options struct {
	Pool *pgxpool.Pool
	SFU  voice.Control
	IDs  *snowflake.Generator
	// Events publishes VOICE_STATE_UPDATE and VOICE_STATE_DELETE after their commits; Bus carries the
	// re-checks guilds publishes.
	Events           *dispatch.Publisher
	Bus              events.Bus
	RateLimitBackend ratelimit.Backend
	// TURNSecret signs the credential a joiner is answered with, and TURNURLs are where clients reach
	// TURN. No URL means no TURN is offered, and a client reaches the SFU directly or not at all.
	TURNSecret []byte
	TURNURLs   []string
	Logger     zerolog.Logger
	Now        func() time.Time
}

// Coordinator is the voice half of op 4. Safe for concurrent use.
type Coordinator struct {
	opts    Options
	q       *db.Queries
	limiter *ratelimit.Limiter
	log     zerolog.Logger
	sub     events.Subscription

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// closed is set by Close before it waits: nothing adds to wg afterwards, a WaitGroup's Add racing its
	// Wait from zero being a misuse.
	closed bool
	// calls are the established calls this process holds, by session, and joining the joins under way.
	calls   map[string]*call
	joining map[string]*call
	// inGuild counts both, per guild, so a re-check of a guild with nothing here costs a map read.
	inGuild  map[snowflake.ID]int
	rechecks map[snowflake.ID]*recheckState
}

// call is one connection this process provisioned, or is provisioning.
type call struct {
	conn, issued, guild, channel, user snowflake.ID
	session, device                    string
	// canceled is a join ended while it was under way, and why: the join ends it once its provision is
	// done, rather than leave a call nobody holds.
	canceled string
	// rechecked is a join a re-check kept while it was under way, which asks for another once established.
	rechecked bool
}

type recheckState struct{ again bool }

// New builds a coordinator. It reads nothing until Sweep, which runs once migrations have.
func New(opts Options) (*Coordinator, error) {
	if opts.Pool == nil || opts.SFU == nil || opts.IDs == nil {
		return nil, errors.New("coordinator: Pool, SFU and IDs are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	limiter, err := ratelimit.New(ratelimit.Options{Rate: joinRate, Bucket: "voice-join", Backend: opts.RateLimitBackend})
	if err != nil {
		return nil, err
	}
	q := db.New(opts.Pool)
	runCtx, cancel := context.WithCancel(context.Background())
	c := &Coordinator{
		opts: opts, q: q, limiter: limiter, ctx: runCtx, cancel: cancel,
		log:      opts.Logger.With().Str("subsystem", "voice").Logger(),
		calls:    map[string]*call{},
		joining:  map[string]*call{},
		inGuild:  map[snowflake.ID]int{},
		rechecks: map[snowflake.ID]*recheckState{},
	}
	if opts.Bus != nil {
		if c.sub, err = opts.Bus.Subscribe(dispatch.VoiceTopic, c.onRecheck); err != nil {
			cancel()
			return nil, fmt.Errorf("coordinator: subscribing to voice re-checks: %w", err)
		}
	}
	// A call ends with its connection, however the SFU came to close it (voice.Closes).
	if closes, ok := opts.SFU.(voice.Closes); ok {
		closes.OnClose(c.ConnectionClosed)
	}
	return c, nil
}

// Sweep deletes every voice_states row, before the instance takes its first join: a single process held
// every call they describe, and none survived its restart. On the flagship's replicas the sweep is per
// replica (Phase P).
func (c *Coordinator) Sweep(ctx context.Context) error {
	if err := c.q.DeleteAllVoiceStates(ctx); err != nil {
		return fmt.Errorf("coordinator: clearing the voice states of a previous run: %w", err)
	}
	return nil
}

// Close stops taking re-checks and waits for those running.
//
// A call still up is left to the SFU's own close, which follows, and its row to the next start's Sweep.
func (c *Coordinator) Close() {
	if c.sub != nil {
		c.sub.Unsubscribe()
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.wg.Wait()
}

// Join answers op 4's join for a session: VOICE_SERVER_UPDATE and its payload, or VOICE_JOIN_REFUSED and
// its code. A refusal provisions nothing.
func (c *Coordinator) Join(
	ctx context.Context, userID snowflake.ID, sessionID, deviceID string, v gatewayproto.VoiceStateUpdate,
) (string, any) {
	channelID, err := snowflake.Parse(*v.ChannelID)
	refuse := func(code string) (string, any) {
		return gatewayproto.EventVoiceJoinRefused,
			gatewayproto.VoiceJoinRefused{Nonce: v.Nonce, ChannelID: *v.ChannelID, Code: code}
	}
	if err != nil || channelID == 0 {
		return refuse(gatewayproto.VoiceNotFound)
	}

	// The channel's guild, before anything is authorized, so the join can be registered under it: a
	// re-check finds a join registered ahead of the read that authorizes it, and a permission change
	// committed in between is seen by one or the other. Its type is asked only once authorized, so a
	// channel the caller cannot see is not described.
	row, err := c.q.GetChannel(ctx, int64(channelID))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.GuildID == nil) {
		return refuse(gatewayproto.VoiceNotFound)
	}
	if err != nil {
		c.log.Error().Err(err).Msg("a voice join could not read its channel")
		return refuse(gatewayproto.VoiceUnavailable)
	}
	guildID := snowflake.ID(*row.GuildID)

	issued, err := c.opts.IDs.Next()
	if err != nil {
		return refuse(gatewayproto.VoiceUnavailable)
	}
	connID, err := c.opts.IDs.Next()
	if err != nil {
		return refuse(gatewayproto.VoiceUnavailable)
	}
	cl := &call{conn: connID, issued: issued, guild: guildID, channel: channelID, user: userID,
		session: sessionID, device: deviceID}
	if !c.register(cl) {
		return refuse(gatewayproto.VoiceBusy)
	}
	established := false
	defer func() {
		if !established {
			c.unregister(cl)
		}
	}()

	actor := auth.Actor{Kind: auth.ActorUser, UserID: userID}
	if _, _, _, err := guildauth.AuthorizeChannelUnlocked(ctx, c.q, actor, channelID, roles.PermConnectVoice); err != nil {
		switch {
		case errors.Is(err, httpx.ErrNotFound):
			return refuse(gatewayproto.VoiceNotFound)
		case errors.Is(err, httpx.ErrForbidden):
			return refuse(gatewayproto.VoiceForbidden)
		}
		c.log.Error().Err(err).Msg("a voice join could not be authorized")
		return refuse(gatewayproto.VoiceUnavailable)
	}
	if row.Type != ChannelGuildVoice {
		return refuse(gatewayproto.VoiceNotVoice)
	}
	// The re-check's own question, asked of the join, so the two cannot disagree: an Instance Admin who is
	// not a member passes AuthorizeChannelUnlocked on layer 1 and is refused here, since a call is not an
	// act rule 14 records.
	rights, err := guildauth.Voice(ctx, c.q, guildID, channelID, []snowflake.ID{userID})
	if err != nil {
		c.log.Error().Err(err).Msg("a voice join could not resolve its rights")
		return refuse(gatewayproto.VoiceUnavailable)
	}
	mine, ok := rights[userID]
	if !ok || !mine.Connect {
		return refuse(gatewayproto.VoiceForbidden)
	}

	// A full channel is refused before the charge, so somebody retrying until a place frees is not rate
	// limited out of it (M33's /code-review). record counts again under the channel's lock.
	if full, err := c.channelFull(ctx, c.q, row, userID); err != nil {
		c.log.Error().Err(err).Msg("a voice join could not count its channel")
		return refuse(gatewayproto.VoiceUnavailable)
	} else if full {
		return refuse(gatewayproto.VoiceFull)
	}

	// Charged once every check that only reads has passed (M22): a join refused for its channel, its
	// rights or a full channel spends none of the account's budget. What is refused after this, by the SFU
	// or by the count under the lock, has cost a provision and spends it.
	res, err := c.limiter.Allow(ctx, userID.String())
	if err != nil {
		c.log.Error().Err(err).Msg("the voice join limiter could not be read")
		return refuse(gatewayproto.VoiceUnavailable)
	}
	if !res.Allowed {
		return refuse(gatewayproto.VoiceRateLimited)
	}

	if beforeReplace != nil {
		beforeReplace()
	}

	// The session's call and its device's older ones go before the new one is provisioned, so a person
	// switching channels is never in two, and the SFU's per-user bound is not spent on a call being left.
	if !c.replace(ctx, cl) {
		return refuse(gatewayproto.VoiceUnavailable)
	}

	if beforeProvision != nil {
		beforeProvision()
	}

	answer, err := c.opts.SFU.Provision(ctx, channelID, userID, connID, issued, v.Offer)
	if err != nil {
		code := provisionCode(err)
		if code == gatewayproto.VoiceUnavailable {
			c.log.Warn().Err(err).Msg("the SFU could not provision a voice join")
		}
		return refuse(code)
	}

	state, code, err := c.record(ctx, cl, row, mine, *v.SupportsVideo)
	if err != nil || code != "" {
		_ = c.opts.SFU.EvictConnection(context.WithoutCancel(ctx), connID, "")
		if err != nil {
			c.log.Error().Err(err).Msg("a voice join could not be recorded")
			code = gatewayproto.VoiceUnavailable
		}
		return refuse(code)
	}

	// Ended while it was under way: by its session going, a leave, or a re-check finding the right gone.
	// The record stands until now, so the end that canceled it could not have removed it; this does.
	reason, stale := c.establish(cl)
	if reason != "" {
		c.end(context.WithoutCancel(ctx), cl, "")
		if reason == canceledByRecheck {
			return refuse(gatewayproto.VoiceForbidden)
		}
		return refuse(gatewayproto.VoiceUnavailable)
	}
	established = true
	// A re-check that kept this join while it was under way could not write its rights, the row not being
	// there yet, and what it read may be newer than what record did: the guild is checked again now that
	// it is (M33's /code-review, a SPEAK removed as the join was provisioned).
	if stale {
		c.requestRecheck(guildID)
	}

	server := gatewayproto.VoiceServer{
		Nonce: v.Nonce, GuildID: guildID.String(), ChannelID: channelID.String(), ConnectionID: connID.String(),
		Answer: answer, ICEServers: []gatewayproto.ICEServer{}, Bitrate: Bitrate(row.Bitrate),
		Mute: state.Mute, Deaf: state.Deaf, Suppress: state.Suppress,
	}
	if len(c.opts.TURNURLs) > 0 {
		cred := turn.Mint(c.opts.TURNSecret, userID, c.opts.Now())
		server.ICEServers = []gatewayproto.ICEServer{{
			URLs: c.opts.TURNURLs, Username: cred.Username, Credential: cred.Password,
		}}
	}
	return gatewayproto.EventVoiceServerUpdate, server
}

// Bitrate is a channel's bitrate as a caller is told it.
func Bitrate(stored *int32) int {
	if stored == nil {
		return DefaultBitrate
	}
	return min(max(int(*stored), MinBitrate), MaxBitrate)
}

// provisionCode is what a refused provision tells the client.
func provisionCode(err error) string {
	switch {
	case errors.Is(err, voice.ErrFull):
		return gatewayproto.VoiceFull
	case errors.Is(err, voice.ErrBadOffer):
		return gatewayproto.VoiceBadOffer
	case errors.Is(err, voice.ErrEvicted):
		// A re-check's cutoff refused it: the right was lost while the join was under way.
		return gatewayproto.VoiceForbidden
	}
	return gatewayproto.VoiceUnavailable
}

// channelFull reports whether user_limit leaves no place for userID. An account already in the channel on
// another machine takes no new place.
func (c *Coordinator) channelFull(ctx context.Context, q *db.Queries, row db.Channel, userID snowflake.ID) (bool, error) {
	if row.UserLimit == nil || *row.UserLimit <= 0 {
		return false, nil
	}
	others, err := q.CountVoiceChannelUsers(ctx, db.CountVoiceChannelUsersParams{
		ChannelID: row.ID, UserID: int64(userID),
	})
	if err != nil {
		return false, err
	}
	return others >= int64(*row.UserLimit), nil
}

// record writes the call's row and tells the channel, under the channel's lock so user_limit holds against
// a join racing this one. A code is a refusal decided here: the channel filled, or went.
func (c *Coordinator) record(
	ctx context.Context, cl *call, ch db.Channel, rights guildauth.VoiceRights, supportsVideo bool,
) (db.VoiceState, string, error) {
	var out db.VoiceState
	var code string
	err := database.RunInTx(ctx, c.opts.Pool, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockVoiceChannel(ctx, int64(cl.channel)); err != nil {
			return err
		}
		full, err := c.channelFull(ctx, q, ch, cl.user)
		if err != nil {
			return err
		}
		if full {
			code = gatewayproto.VoiceFull
			return nil
		}
		flags, err := memberFlags(ctx, q, cl.guild, []snowflake.ID{cl.user})
		if err != nil {
			return err
		}
		out, err = q.InsertVoiceState(ctx, db.InsertVoiceStateParams{
			ConnectionID: int64(cl.conn), GuildID: int64(cl.guild), ChannelID: int64(cl.channel),
			UserID: int64(cl.user), SessionID: cl.session, DeviceID: cl.device,
			Mute: flags[cl.user].mute, Deaf: flags[cl.user].deaf, Suppress: !rights.Speak,
			SupportsVideo: supportsVideo,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			// The channel or the guild was deleted since the join was authorized.
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				code = gatewayproto.VoiceNotFound
				return errNothingWritten
			}
			return err
		}
		return c.queueState(ctx, out)
	})
	if errors.Is(err, errNothingWritten) {
		err = nil
	}
	return out, code, err
}

// errNothingWritten rolls a transaction back that decided a refusal rather than failed.
var errNothingWritten = errors.New("coordinator: nothing written")

type flag struct{ mute, deaf bool }

// memberFlags are the server mute and deafen of users who are members.
func memberFlags(ctx context.Context, q *db.Queries, guildID snowflake.ID, users []snowflake.ID) (map[snowflake.ID]flag, error) {
	ids := make([]int64, len(users))
	for i, u := range users {
		ids[i] = int64(u)
	}
	rows, err := q.GetMembersVoiceFlags(ctx, db.GetMembersVoiceFlagsParams{GuildID: int64(guildID), UserIds: ids})
	if err != nil {
		return nil, err
	}
	out := make(map[snowflake.ID]flag, len(rows))
	for _, r := range rows {
		out[snowflake.ID(r.UserID)] = flag{mute: r.Mute, deaf: r.Deaf}
	}
	return out, nil
}

// Leave ends the session's call, and a join it has under way.
func (c *Coordinator) Leave(ctx context.Context, userID snowflake.ID, sessionID string) {
	if cl := c.take(sessionID, canceledByLeave); cl != nil {
		c.end(ctx, cl, "")
	}
}

// Ended ends the call of a session the gateway dropped. It returns at once and ends the call on a goroutine
// of its own, bounded, since the gateway calls it holding nothing it should wait on.
func (c *Coordinator) Ended(userID snowflake.ID, sessionID string) {
	cl := c.take(sessionID, canceledBySession)
	if cl == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), endWithin)
		defer cancel()
		c.end(ctx, cl, endedSession)
	}()
}

// Why a join under way was ended.
const (
	canceledByLeave   = "left"
	canceledBySession = "session"
	canceledByRecheck = "recheck"
	canceledByReplace = "replace"
	canceledByClose   = "closed"
)

// register records a join under way for its session, or reports one is already under way.
func (c *Coordinator) register(cl *call) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, busy := c.joining[cl.session]; busy {
		return false
	}
	c.joining[cl.session] = cl
	c.inGuild[cl.guild]++
	return true
}

// unregister forgets a join that did not become a call.
func (c *Coordinator) unregister(cl *call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.joining[cl.session] == cl {
		delete(c.joining, cl.session)
		c.forgetLocked(cl)
	}
}

// establish makes a join the session's call, unless it was canceled meanwhile, and then says why. stale is
// a join a re-check saw while it was under way.
func (c *Coordinator) establish(cl *call) (reason string, stale bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl.canceled != "" {
		return cl.canceled, false
	}
	delete(c.joining, cl.session)
	c.calls[cl.session] = cl
	return "", cl.rechecked
}

// take removes the session's call and returns it, and cancels a join it has under way.
func (c *Coordinator) take(sessionID, why string) *call {
	c.mu.Lock()
	defer c.mu.Unlock()
	if j := c.joining[sessionID]; j != nil && j.canceled == "" {
		j.canceled = why
	}
	cl := c.calls[sessionID]
	if cl == nil {
		return nil
	}
	delete(c.calls, sessionID)
	c.forgetLocked(cl)
	return cl
}

func (c *Coordinator) forgetLocked(cl *call) {
	if c.inGuild[cl.guild]--; c.inGuild[cl.guild] <= 0 {
		delete(c.inGuild, cl.guild)
	}
}

// replace ends the joining session's own call, and every call its device holds on any session: a restarted
// daemon's new session takes over from the one its previous run left behind for the resume window.
//
// The device's newest join wins, by its connection id, which is minted as the join starts and orders in
// time. A join the device has under way on another session is canceled, so it does not establish a call
// after this one: its row may not be written yet, so the listing below cannot find it. A join that finds
// the device has a newer one, under way or established, ends nothing and reports false: it is the one
// replaced (M33's /code-review). The ends run at once, since one that says why waits a moment for the
// reason to leave, and this is on the join's path.
func (c *Coordinator) replace(ctx context.Context, joining *call) bool {
	sameDevice := func(o *call) bool {
		return o != joining && o.user == joining.user && o.device == joining.device
	}
	c.mu.Lock()
	for _, set := range []map[string]*call{c.joining, c.calls} {
		for _, o := range set {
			if sameDevice(o) && o.conn > joining.conn {
				c.mu.Unlock()
				return false
			}
		}
	}
	for _, j := range c.joining {
		if sameDevice(j) && j.canceled == "" {
			j.canceled = canceledByReplace
		}
	}
	c.mu.Unlock()
	ending := map[*call]string{}
	if cl := c.take(joining.session, ""); cl != nil {
		ending[cl] = ""
	}
	rows, err := c.q.ListVoiceStatesOfDevice(ctx, db.ListVoiceStatesOfDeviceParams{
		UserID: int64(joining.user), DeviceID: joining.device,
	})
	if err != nil {
		c.log.Error().Err(err).Msg("a voice join could not list its device's calls")
	}
	for _, r := range rows {
		// A newer call of the device's, on another replica, is not this join's to end.
		if snowflake.ID(r.ConnectionID) > joining.conn {
			return false
		}
		// Taken from this process's books if it is one of its sessions; a call on another replica is that
		// replica's to forget, and its end finds the row gone.
		if r.SessionID != joining.session {
			c.take(r.SessionID, canceledByReplace)
		}
		ending[&call{conn: snowflake.ID(r.ConnectionID), guild: snowflake.ID(r.GuildID),
			channel: snowflake.ID(r.ChannelID), user: snowflake.ID(r.UserID)}] = endedReplaced
	}
	c.endEach(ctx, ending)
	return true
}

// endEach ends each call, saying its reason, all at once: telling a client why waits a moment for the
// reason to leave, and many ends would otherwise wait once each.
func (c *Coordinator) endEach(ctx context.Context, ending map[*call]string) {
	var wg sync.WaitGroup
	for cl, why := range ending {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.end(ctx, cl, why)
		}()
	}
	wg.Wait()
}

// ConnectionClosed is the SFU telling the coordinator it closed a connection, by itself or as asked. A call
// this process still holds with that connection ended without anybody here ending it: ICE gave up, the
// provision was never connected, the peer broke a bound or went away. Its row and the channel's view go at
// once, rather than when its session does (M33's /code-review). A join under way is canceled and ends
// itself. Called on the SFU's goroutine, so it returns at once.
func (c *Coordinator) ConnectionClosed(conn snowflake.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	for _, j := range c.joining {
		if j.conn == conn && j.canceled == "" {
			j.canceled = canceledByClose
		}
	}
	var cl *call
	for _, held := range c.calls {
		if held.conn == conn {
			cl = held
			break
		}
	}
	if cl == nil {
		return
	}
	delete(c.calls, cl.session)
	c.forgetLocked(cl)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), endWithin)
		defer cancel()
		c.end(ctx, cl, "")
	}()
}

// Why a call was ended, as the SFU tells its client before it closes the connection, in words the client
// shows the person (M33's manual pass, where every one of them read "the voice server closed the call").
// A call its client left is told nothing.
const (
	endedChannelGone = "the voice channel was deleted"
	endedNotMember   = "you are no longer a member of this guild"
	endedNoConnect   = "you may no longer join this voice channel"
	endedSession     = "this device's session with the instance ended"
	endedReplaced    = "this device joined a voice channel again"
)

// end closes a call on the SFU, saying why unless why is empty, deletes its row and tells the channel. Each
// step is idempotent: an end racing another, or a cascade, finds the row gone and says nothing.
func (c *Coordinator) end(ctx context.Context, cl *call, why string) {
	if err := c.opts.SFU.EvictConnection(ctx, cl.conn, why); err != nil && !errors.Is(err, voice.ErrClosed) {
		c.log.Warn().Err(err).Msg("the SFU could not end a call")
	}
	err := database.RunInTx(ctx, c.opts.Pool, func(ctx context.Context, tx pgx.Tx) error {
		row, err := db.New(tx).DeleteVoiceState(ctx, int64(cl.conn))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return c.queueDeleted(ctx, row)
	})
	if err != nil {
		c.log.Error().Err(err).Msg("a call's voice state could not be removed")
	}
}

// queueState tells the channel's viewers about a row, after the transaction commits (rule 5).
func (c *Coordinator) queueState(ctx context.Context, r db.VoiceState) error {
	return c.opts.Events.Queue(ctx, dispatch.Event{
		Type: gatewayproto.EventVoiceStateUpdate, Audience: dispatch.Guild,
		GuildID: snowflake.ID(r.GuildID), ChannelID: snowflake.ID(r.ChannelID),
	}, voicestate.Of(r))
}

func (c *Coordinator) queueDeleted(ctx context.Context, r db.VoiceState) error {
	return c.opts.Events.Queue(ctx, dispatch.Event{
		Type: gatewayproto.EventVoiceStateDelete, Audience: dispatch.Guild,
		GuildID: snowflake.ID(r.GuildID), ChannelID: snowflake.ID(r.ChannelID),
	}, voicestate.DeletedOf(r))
}
