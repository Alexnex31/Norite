// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/internal/updater"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// updateFixture is the daemon's own requests over an updater for a development build in an empty state
// directory: one that installs nothing and, asked to check, refuses before it asks the network anything.
func updateFixture(t *testing.T) (*localRequests, *atomic.Int32) {
	t.Helper()
	var changed atomic.Int32
	up := updater.New(updater.Options{Version: "dev", StateDir: t.TempDir(), Log: zerolog.Nop(),
		Restart: func() { t.Error("a development build asked to restart") },
		Changed: func() { changed.Add(1) }})
	return &localRequests{log: zerolog.Nop(), update: up}, &changed
}

func TestTheUpdateRequestsAnswerAStatus(t *testing.T) {
	l, changed := updateFixture(t)
	ask := func(method, path string) ipc.Response {
		return l.Do(context.Background(), ipc.Request{Method: method, Path: path})
	}

	resp := ask(http.MethodGet, ipc.PathUpdate)
	require.Nil(t, resp.Error)
	require.Equal(t, http.StatusOK, *resp.Status)
	matchesDefinition(t, "UpdateStatus", resp.Body)
	var status ipc.UpdateStatus
	require.NoError(t, json.Unmarshal(resp.Body, &status))
	require.Equal(t, ipc.UpdateStatus{Version: "dev", Mode: ipc.UpdateNotify, Channel: ipc.ChannelStable,
		Reason: status.Reason}, status)
	require.Contains(t, status.Reason, "development build")

	// A choice the daemon can keep is kept, said to whoever watches, and read back.
	resp = ask(http.MethodPost, ipc.PathUpdateMode+ipc.UpdateOff)
	require.Nil(t, resp.Error)
	matchesDefinition(t, "UpdateStatus", resp.Body)
	require.EqualValues(t, 1, changed.Load())
	resp = ask(http.MethodPost, ipc.PathUpdateChannel+ipc.ChannelPrerelease)
	require.Nil(t, resp.Error)
	require.NoError(t, json.Unmarshal(ask(http.MethodGet, ipc.PathUpdate).Body, &status))
	require.Equal(t, ipc.UpdateOff, status.Mode)
	require.Equal(t, ipc.ChannelPrerelease, status.Channel)
}

func TestTheUpdateRequestsRefuseWhatTheyCannotDo(t *testing.T) {
	l, _ := updateFixture(t)
	cases := []struct {
		method, path, code string
	}{
		// Reading is GET and changing is POST.
		{http.MethodPost, ipc.PathUpdate, ipc.RelayBadRequest},
		{http.MethodDelete, ipc.PathUpdate, ipc.RelayBadRequest},
		{http.MethodGet, ipc.PathUpdateCheck, ipc.RelayBadRequest},
		{http.MethodGet, ipc.PathUpdateApply, ipc.RelayBadRequest},
		{http.MethodGet, ipc.PathUpdateMode + ipc.UpdateOff, ipc.RelayBadRequest},
		// A path that is none of them.
		{http.MethodPost, ipc.PathUpdate + "/rollback", ipc.RelayBadRequest},
		{http.MethodPost, ipc.PathUpdate + "/", ipc.RelayBadRequest},
		// A word that is not one of the words.
		{http.MethodPost, ipc.PathUpdateMode + "always", ipc.RelayConflict},
		{http.MethodPost, ipc.PathUpdateMode, ipc.RelayConflict},
		{http.MethodPost, ipc.PathUpdateMode + "off/../auto", ipc.RelayConflict},
		{http.MethodPost, ipc.PathUpdateChannel + "nightly", ipc.RelayConflict},
		// What this daemon cannot do, said with its reason and not as a failure.
		{http.MethodPost, ipc.PathUpdateMode + ipc.UpdateAuto, ipc.RelayConflict},
		{http.MethodPost, ipc.PathUpdateApply, ipc.RelayConflict},
		{http.MethodPost, ipc.PathUpdateCheck, ipc.RelayConflict},
	}
	for _, tc := range cases {
		resp := l.Do(context.Background(), ipc.Request{Method: tc.method, Path: tc.path})
		require.NotNil(t, resp.Error, "%s %s", tc.method, tc.path)
		require.Equal(t, tc.code, resp.Error.Code, "%s %s: %s", tc.method, tc.path, resp.Error.Message)
		require.Nil(t, resp.Status)
	}
}

// A daemon with no updater answers these as it answers any path it does not know.
func TestWithNoUpdaterTheRequestsAreNobodys(t *testing.T) {
	l := &localRequests{log: zerolog.Nop()}
	resp := l.Do(context.Background(), ipc.Request{Method: http.MethodGet, Path: ipc.PathUpdate})
	require.NotNil(t, resp.Error)
}

// A release host that cannot be reached is said as that, and not as a refusal: `norite update check`
// exits 3 for it, where "nothing newer" is not an error at all and a refusal is 4.
func TestAReleaseHostThatCannotBeReachedIsNotARefusal(t *testing.T) {
	nowhere := updater.Source{ListURL: "http://127.0.0.1:1/releases", DownloadBase: "http://127.0.0.1:1/download",
		UserAgent: "test", Client: http.DefaultClient}
	up := updater.New(updater.Options{Version: "0.1.0", StateDir: t.TempDir(), Log: zerolog.Nop(), Source: &nowhere})
	l := &localRequests{log: zerolog.Nop(), update: up}
	resp := l.Do(context.Background(), ipc.Request{Method: http.MethodPost, Path: ipc.PathUpdateCheck})
	require.NotNil(t, resp.Error)
	require.Equal(t, ipc.RelayUnreachable, resp.Error.Code)
}
