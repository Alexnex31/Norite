package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// The account-facing view of its own sessions.
//
// # Why a "session" here is a device and not a row
//
// The sessions table stores one row per generation of a refresh-token family: rotation inserts a
// successor and revokes its predecessor, so a client on a fifteen-minute access token produces a new row
// four times an hour. Presenting those rows to a person would mean a list that grows while they read it,
// and ids that are stale by the time they click one.
//
// So a listed session is a *device family*, keyed by device_id, and revoking one revokes the family. The
// schema said so before this milestone did: sessions_user_device_idx's comment names "list my sessions
// (M11)" as its second reader, and the index is on (user_id, device_id) precisely because that is the
// grain a person thinks in.

// SessionDevice is one device signed in to an account.
type SessionDevice struct {
	// ID is the newest live session row in this family, and what identifies the device to the revoke
	// endpoint. A snowflake rather than the device_id itself: device_id is client-chosen text that would
	// land in the request log of every revoke (the reasoning M10 applied to invite codes), while a
	// snowflake authorizes nothing and is safe in a path.
	ID snowflake.ID
	// Name is what the client called this machine at sign-in, or empty if it did not say. Client-supplied
	// and therefore untrusted: every renderer of it applies rule 19.
	Name string
	// Address is where the session was last seen from. May be unset — a session created over a Unix socket
	// or by a test has none.
	Address *netip.Addr
	// FirstSeen is when this device first signed in, across the whole family. Not the newest row's
	// created_at, which is merely the last rotation.
	FirstSeen time.Time
	LastUsed  time.Time
	ExpiresAt time.Time
	// Current reports whether this is the device the request came from.
	Current bool
}

// ListSessionDevices returns the devices signed in to an account, most recently used first.
//
// currentSessionID is the sid claim of the access token making the request, used only to mark one entry.
// It is resolved through the device it belongs to rather than compared row-to-row, because the row it
// names may have been rotated away since the token was minted — see currentDeviceID.
func (s *Service) ListSessionDevices(ctx context.Context, userID, currentSessionID snowflake.ID,
) ([]SessionDevice, error) {
	rows, err := s.queries.ListSessionDevicesForUser(ctx, int64(userID))
	if err != nil {
		return nil, fmt.Errorf("listing session devices: %w", err)
	}

	currentDevice, err := s.currentDeviceID(ctx, userID, currentSessionID)
	if err != nil {
		return nil, err
	}

	out := make([]SessionDevice, 0, len(rows))
	for _, row := range rows {
		device := SessionDevice{
			ID:        snowflake.ID(row.ID),
			Address:   row.IpAddress,
			FirstSeen: row.FirstSeen.Time,
			LastUsed:  row.LastUsedAt.Time,
			ExpiresAt: row.ExpiresAt.Time,
			Current:   row.DeviceID == currentDevice,
		}
		if row.DeviceName != nil {
			device.Name = *row.DeviceName
		}
		out = append(out, device)
	}
	return out, nil
}

// RevokeSessionDevice signs one device out, family and all.
//
// Takes the id of a session row and revokes every live session sharing its device_id, which is what makes
// the action mean what the listing showed. Revoking the caller's own device is allowed: that is "sign out
// here", and refusing it would be a special case nobody asked for.
func (s *Service) RevokeSessionDevice(ctx context.Context, userID, sessionID snowflake.ID) error {
	session, err := s.queries.GetSessionByID(ctx, int64(sessionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("looking up session: %w", err)
	}

	// Not found rather than forbidden. A session id belonging to somebody else must not be confirmable as
	// existing — the two answers together would let anyone with a list of snowflakes learn which name real
	// sessions, and a snowflake carries its creation time.
	if session.UserID != int64(userID) {
		return ErrNotFound
	}

	if _, err := s.queries.RevokeSessionsForDevice(ctx, db.RevokeSessionsForDeviceParams{
		UserID:   int64(userID),
		DeviceID: session.DeviceID,
	}); err != nil {
		return fmt.Errorf("revoking device sessions: %w", err)
	}
	return nil
}

// RevokeOtherSessions signs out everything except the device the request came from.
//
// Everything: sessions on other devices, and also every API token, outstanding OAuth exchange code and
// approved device authorization on the account — the whole primitive, minus this device. The API tokens
// are the part worth being deliberate about, because somebody kicking a lost laptop also stops their bots.
// It is the answer chosen at M11 rather than an accident: one action that cannot under-clean beats two
// that a person has to know to run in order, and the response says what it did.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID, currentSessionID snowflake.ID,
) (RevocationResult, error) {
	currentDevice, err := s.currentDeviceID(ctx, userID, currentSessionID)
	if err != nil {
		return RevocationResult{}, err
	}
	// In a transaction, like every other caller of the primitive.
	//
	// The four statements are one action, and a partial one is worse than none: if the API-token revoke
	// fails after the session revoke has committed, the caller gets a 500 that reads as "nothing happened"
	// while every other device is in fact signed out and every API token — the part this endpoint exists
	// to report — is still live. The intruder's credential would survive the action taken to kill it.
	//
	// revokeEverything takes a *db.Queries precisely so it can be composed like this. The reset path always
	// did; this one shipped without, which is the sort of thing only a second reader notices.
	var out RevocationResult
	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		out, err = revokeEverything(ctx, s.queries.WithTx(tx), int64(userID),
			RevocationScope{KeepDeviceID: currentDevice})
		return err
	})
	if err != nil {
		return RevocationResult{}, err
	}
	return out, nil
}

// requireLiveDevice refuses an action from a device that has already been signed out.
//
// This is the one place the stateless access token is checked against session state, and the line is drawn
// deliberately. §17.10 accepts that an access token keeps working for up to fifteen minutes after its
// session is revoked, because closing that window means a database lookup on every authenticated request —
// on the hottest path in the API, against the shortest-lived credential here.
//
// Reading a profile inside that window is what the trade buys. *Undoing a revocation* inside it is not:
// without this check, a device signed out by "sign out everywhere else" can spend its remaining minutes
// calling the same endpoint and signing out the device that signed it out — taking the account's API
// tokens with it each time. The owner wins eventually, because only they can authenticate afresh, but the
// operation whose entire promise is "this took effect" would have quietly not.
//
// So the rule is narrow and statable: an endpoint whose purpose is revocation does not accept a credential
// whose own session has been revoked. It costs one indexed lookup on an endpoint called approximately
// never, and nothing on the path §17.10 is about.
func (s *Service) requireLiveDevice(ctx context.Context, userID, sessionID snowflake.ID) error {
	device, err := s.currentDeviceID(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	return s.requireLiveDeviceNamed(ctx, userID, device)
}

// requireLiveDeviceNamed is requireLiveDevice for a caller that has already resolved the device.
func (s *Service) requireLiveDeviceNamed(ctx context.Context, userID snowflake.ID, device string) error {
	// No device at all: an access token naming a session this instance has no record of. Not reachable in
	// ordinary use — the row outlives the token that names it by weeks — so refusing is the safe reading.
	if device == "" {
		return ErrSessionSignedOut
	}

	live, err := s.queries.CountLiveSessionsForDevice(ctx, db.CountLiveSessionsForDeviceParams{
		UserID:   int64(userID),
		DeviceID: device,
	})
	if err != nil {
		return fmt.Errorf("counting live sessions for the acting device: %w", err)
	}
	if live == 0 {
		return ErrSessionSignedOut
	}
	return nil
}

// currentDeviceID resolves the access token's session claim to the device it belongs to.
//
// The lookup deliberately accepts a revoked row. An access token lives fifteen minutes and names the
// session it was minted from; any refresh inside that window revokes that row while leaving the token
// perfectly valid. Reading through a query that hid revoked rows would report "no current device" for
// every client that had refreshed recently — and RevokeOtherSessions would then spare nothing and sign the
// caller out of itself, in the one operation whose entire promise is that it does not.
//
// An empty result is not an error: an API-token actor carries no session, and a session id that names
// nothing at all is a token this instance did not mint. Both mean "no device to spare", and the caller
// decides what that is worth.
func (s *Service) currentDeviceID(ctx context.Context, userID, sessionID snowflake.ID) (string, error) {
	if sessionID == 0 {
		return "", nil
	}

	session, err := s.queries.GetSessionByID(ctx, int64(sessionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("looking up the current session: %w", err)
	}
	if session.UserID != int64(userID) {
		return "", nil
	}
	return session.DeviceID, nil
}
