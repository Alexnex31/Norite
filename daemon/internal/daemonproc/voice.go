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

// signedInAccount is the account a call is joined as: the session's, once it names one and is not signed
// out.
func signedInAccount(src *session.Source) func() (string, bool) {
	return func() (string, bool) {
		standing, account := src.Status()
		if standing == session.SignedOut || account.UserID == "" {
			return "", false
		}
		return account.UserID, true
	}
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
