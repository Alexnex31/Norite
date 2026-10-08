// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

type recorded struct {
	answer
	method, path string
	body         any
}

func (r *recorded) Do(_ context.Context, method, path string, body any) (ipc.Result, error) {
	r.method, r.path, r.body = method, path, body
	return r.res, r.err
}

// A local request is a POST with no body, and its 200 decodes into what was asked for.
func TestALocalRequestDecodesTheDaemonsAnswer(t *testing.T) {
	c := &recorded{answer: answer{res: ipc.Result{Status: 200, Body: json.RawMessage(`{"split":true,"files":["a","b"]}`)}}}
	var out ipc.ConfigToggle
	require.NoError(t, Local(context.Background(), c, ipc.PathConfigSplit, &out))
	assert.True(t, out.Split)
	assert.Equal(t, []string{"a", "b"}, out.Files)
	assert.Equal(t, "POST", c.method)
	assert.Equal(t, ipc.PathConfigSplit, c.path)
	assert.Nil(t, c.body)
}

// Every way a local request can end, and the exit code a script reads for it. The daemon's refusal is a
// refusal (4) in the daemon's words, made safe to print; its failure is a failure (1). Anything that can
// only mean the daemon did not know the request and relayed it instead is "restart the daemon" (3), never
// the instance's answer or a hint to log in.
func TestEveryLocalOutcomeEndsInItsExitCode(t *testing.T) {
	relay := func(code, msg string) answer { return answer{err: &ipc.RelayError{Code: code, Message: msg}} }
	for name, tc := range map[string]struct {
		a     answer
		check func(t *testing.T, err error)
	}{
		"the daemon refused": {relay(ipc.RelayConflict, "the config is already split\x1b[2J"), func(t *testing.T, err error) {
			var refused *clierr.RefusedError
			require.ErrorAs(t, err, &refused)
			assert.Contains(t, err.Error(), "already split")
			assert.NotContains(t, err.Error(), "\x1b")
		}},
		"the daemon failed": {relay(ipc.RelayFailed, "disk full"), func(t *testing.T, err error) {
			var refused *clierr.RefusedError
			var unavailable *clierr.UnavailableError
			assert.False(t, errors.As(err, &refused) || errors.As(err, &unavailable), "exit 1: %v", err)
			assert.Contains(t, err.Error(), "disk full")
		}},
		"an older daemon, signed in, relayed it and the instance answered": {
			answer{res: ipc.Result{Status: 404, Body: json.RawMessage(`{"error":{"code":"not_found","message":"no"}}`)}},
			older},
		"an older daemon, signed out":                  {relay(ipc.RelayNotSignedIn, "run norite login"), older},
		"an older daemon whose relay refused the path": {relay(ipc.RelayRefused, "no"), older},
		"a daemon with nothing at that path":           {relay(ipc.RelayBadRequest, "no"), older},
		"the daemon went away": {answer{err: ipc.ErrClosed}, func(t *testing.T, err error) {
			var unavailable *clierr.UnavailableError
			require.ErrorAs(t, err, &unavailable)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var out ipc.ConfigToggle
			tc.check(t, Local(context.Background(), tc.a, ipc.PathConfigSplit, &out))
		})
	}
}

func older(t *testing.T, err error) {
	var unavailable *clierr.UnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Contains(t, err.Error(), "norite daemon restart")
	assert.NotContains(t, err.Error(), "login")
}
