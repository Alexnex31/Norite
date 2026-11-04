// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/internal/voiceworker"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// signedInAccount is the sign-in a call is joined under: the session's account, and the session's own
// signal that the sign-in is over. The session's and not the gateway connection's, which hears of a sign-out
// only while connected: one made while the instance is unreachable ended nothing until the next sign-in, and
// the call, whose voice server may be reachable all the same, went on as an account nobody was signed in as.
// A daemon that has not read its sign-in yet is pending, never signed out, or a join would send somebody to
// `norite login` over a good sign-in (M22's automation.ErrSignInPending).
func signedInAccount(src *session.Source) func() (voiceworker.SignIn, error) {
	return func() (voiceworker.SignIn, error) {
		standing, account := src.Status()
		switch {
		case standing == session.SignedOut:
			return voiceworker.SignIn{}, voiceworker.ErrSignedOut
		case account.UserID == "":
			return voiceworker.SignIn{}, voiceworker.ErrSignInPending
		}
		// Asked after the status: a sign-in that ended in between is a channel already closed.
		return voiceworker.SignIn{Account: account.UserID, Ended: src.Ended(account.Generation)}, nil
	}
}

// noVoice is the daemon's voice calls when the credential store could not be opened: answered, with the
// reason, rather than refused as a request this daemon does not know.
func noVoice(version string) *voiceworker.Voice {
	return voiceworker.NewVoice(voiceworker.VoiceOptions{Version: version,
		Unavailable: "the daemon could not open its credential store; its log says why"})
}

// handlesVoice is whether path is one of the requests about voice calls.
func handlesVoice(path string) bool {
	return path == ipc.PathVoice || strings.HasPrefix(path, ipc.PathVoice+"/")
}

// voiceRequested answers the requests under ipc.PathVoice (M28).
func (l *localRequests) voiceRequested(ctx context.Context, req ipc.Request) ipc.Response {
	const wrongMethod = "a call is read with GET and joined or left with POST"
	var (
		out ipc.VoiceStatus
		err error
	)
	channel, joining := ipc.VoiceJoinChannel(req.Path)
	switch {
	case req.Path == ipc.PathVoice && req.Method != http.MethodGet:
		return ipc.Failure(ipc.RelayBadRequest, wrongMethod)
	case req.Path == ipc.PathVoice:
		out = l.voice.Status()
	case req.Method != http.MethodPost:
		return ipc.Failure(ipc.RelayBadRequest, wrongMethod)
	case req.Path == ipc.PathVoiceLeave:
		out = l.voice.Leave()
	case joining:
		out, err = l.voice.Join(ctx, channel)
	default:
		return ipc.Failure(ipc.RelayBadRequest, "the daemon answers no request of its own at that path")
	}
	var refused *voiceworker.Refusal
	switch {
	case errors.As(err, &refused) && refused.Pending:
		return ipc.Failure(ipc.RelayUnreachable, refused.Msg)
	case errors.As(err, &refused) && refused.Failed:
		return ipc.Failure(ipc.RelayFailed, refused.Msg)
	case errors.As(err, &refused):
		return ipc.Failure(ipc.RelayConflict, refused.Msg)
	case err != nil:
		// The asker went away; the join goes on, and the status says how it went.
		return ipc.Failure(ipc.RelayFailed, "the join is still under way; ask again with `norite voice status`")
	}
	body, err := json.Marshal(out)
	if err != nil {
		return ipc.Failure(ipc.RelayFailed, "the daemon could not encode its answer")
	}
	status := http.StatusOK
	return ipc.Response{Status: &status, Body: body}
}
