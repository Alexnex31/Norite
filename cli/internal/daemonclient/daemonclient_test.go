// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

type answer struct {
	res ipc.Result
	err error
}

func (a answer) Do(context.Context, string, string, any) (ipc.Result, error) { return a.res, a.err }

func TestEveryOutcomeEndsInItsExitCode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		given answer
		check func(t *testing.T, err error)
	}{
		{"a 2xx", answer{res: ipc.Result{Status: 200, Body: json.RawMessage(`{"id":"1"}`)}},
			func(t *testing.T, err error) { assert.NoError(t, err) }},
		{"a non-member's 404", answer{res: ipc.Result{Status: 404, Body: json.RawMessage(
			`{"error":{"code":"not_found","message":"not \u001b[2Jfound","request_id":"r-1"}}`)}},
			func(t *testing.T, err error) {
				var refused *clierr.RefusedError
				require.ErrorAs(t, err, &refused)
				assert.Equal(t, 404, refused.Status)
				assert.Equal(t, "not_found", refused.Code)
				assert.NotContains(t, refused.Message, "\x1b", "the instance's words are foreign (rule 19)")
				assert.Equal(t, "r-1", refused.RequestID)
			}},
		{"a 403 with no body", answer{res: ipc.Result{Status: 403}},
			func(t *testing.T, err error) {
				var refused *clierr.RefusedError
				require.ErrorAs(t, err, &refused)
				assert.Contains(t, err.Error(), "HTTP 403")
			}},
		{"a 401 after the relay's retry", answer{res: ipc.Result{Status: 401}},
			func(t *testing.T, err error) {
				var unavailable *clierr.UnavailableError
				require.ErrorAs(t, err, &unavailable)
				assert.Contains(t, err.Error(), "norite login")
			}},
		{"a throttle", answer{res: ipc.Result{Status: 429, Body: json.RawMessage(
			`{"error":{"code":"rate_limited","message":"slow down","request_id":"r-2"}}`)}},
			func(t *testing.T, err error) {
				var unavailable *clierr.UnavailableError
				require.ErrorAs(t, err, &unavailable, "not now, rather than no")
			}},
		{"a 502 from a proxy", answer{res: ipc.Result{Status: 502}},
			func(t *testing.T, err error) {
				assert.EqualError(t, err, "the instance failed the request (HTTP 502)")
				var refused *clierr.RefusedError
				assert.False(t, errors.As(err, &refused))
			}},
		{"a daemon signed out", answer{err: &ipc.RelayError{Code: ipc.RelayNotSignedIn, Message: "not signed in"}},
			func(t *testing.T, err error) {
				var unavailable *clierr.UnavailableError
				require.ErrorAs(t, err, &unavailable)
			}},
		{"a refused path", answer{err: &ipc.RelayError{Code: ipc.RelayRefused, Message: "no"}},
			func(t *testing.T, err error) {
				var unavailable *clierr.UnavailableError
				assert.False(t, errors.As(err, &unavailable), "a bug in a verb, not a condition of this machine")
			}},
		{"a daemon that stopped mid-request", answer{err: &ipc.CloseError{Code: ipc.CloseGoingAway, Reason: "stopping"}},
			func(t *testing.T, err error) {
				var unavailable *clierr.UnavailableError
				require.ErrorAs(t, err, &unavailable)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out map[string]any
			tc.check(t, Call(context.Background(), tc.given, "GET", "/guilds/1", nil, &out))
		})
	}
}

// TestNoDaemonIsUnavailableAndSaysWhatToDo: E1, an actionable error and exit 3, and no daemon started.
func TestNoDaemonIsUnavailableAndSaysWhatToDo(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("points the state directory somewhere empty through XDG_STATE_HOME, which is Linux's")
	}
	dir, err := os.MkdirTemp("", "ndc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_STATE_HOME", dir)

	_, err = Connect(context.Background(), "dev")
	var unavailable *clierr.UnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Contains(t, err.Error(), "norite daemon start")
}
