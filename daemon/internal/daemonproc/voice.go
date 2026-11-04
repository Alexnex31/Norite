// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Alexnex31/Norite/daemon/internal/gatewayclient"
	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/internal/voiceworker"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// signedInAccount is the account a call is joined as: the session's. A daemon that has not read its
// sign-in yet is pending, never signed out, or a join would send somebody to `norite login` over a good
// sign-in (M22's automation.ErrSignInPending).
func signedInAccount(src *session.Source) func() (string, error) {
	return func() (string, error) {
		standing, account := src.Status()
		switch {
		case standing == session.SignedOut:
			return "", voiceworker.ErrSignedOut
		case account.UserID == "":
			return "", voiceworker.ErrSignInPending
		}
		return account.UserID, nil
	}
}

// endingCalls is the gateway's sink with the daemon's voice calls behind it: a sign-in that ends takes its
// call with it (M19's rule), as it takes the state. Signed out, revoked or replaced by another account's,
// the call was joined as an account nobody here is signed in as any more. SignInEnded does not wait for the
// worker, since End is called from the gateway's read loop, and it refuses a join still waiting on the
// sign-in that ended.
type endingCalls struct {
	gatewayclient.Sink
	calls *voiceworker.Voice
}

func (e endingCalls) End() {
	e.Sink.End()
	e.calls.SignInEnded()
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
