// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// matchesStopping holds an answer to daemon-ipc.schema.json's Stopping.
func matchesStopping(t *testing.T, body []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "daemon-ipc.schema.json"))
	require.NoError(t, err)
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Contains(t, doc.Defs, "Stopping")
	def, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc.Defs["Stopping"]))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("stopping.json", def))
	schema, err := c.Compile("stopping.json")
	require.NoError(t, err)
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, schema.Validate(inst), "%s", body)
}

// A stop request is answered with the process to wait for, and stops the daemon once however many times
// it is asked.
func TestAStopRequestAnswersWithThePIDAndStopsOnce(t *testing.T) {
	var stops atomic.Int32
	stopped := make(chan struct{}, 2)
	l := &localRequests{log: zerolog.Nop(), stop: func() {
		stops.Add(1)
		stopped <- struct{}{}
	}}

	for range 2 {
		resp := l.Do(context.Background(), ipc.Request{Method: "POST", Path: ipc.PathStop})
		require.Nil(t, resp.Error)
		require.NotNil(t, resp.Status)
		assert.Equal(t, 200, *resp.Status)
		matchesStopping(t, resp.Body)
		var out ipc.Stopping
		require.NoError(t, json.Unmarshal(resp.Body, &out))
		assert.Equal(t, os.Getpid(), out.PID)
	}

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon was never told to stop")
	}
	// Long enough for a second timer to have fired, had one been armed.
	time.Sleep(2 * stopGrace)
	assert.Equal(t, int32(1), stops.Load(), "asked twice, it stops once")
}

// The answer is queued before the stop begins: stopping closes every connection with whatever is queued
// unsent, so a stop begun inside the handler would usually cost the asker its answer.
func TestAStopRequestIsAnsweredBeforeTheDaemonStops(t *testing.T) {
	var stops atomic.Int32
	l := &localRequests{log: zerolog.Nop(), stop: func() { stops.Add(1) }}

	resp := l.Do(context.Background(), ipc.Request{Method: "POST", Path: ipc.PathStop})
	require.Nil(t, resp.Error)
	assert.Zero(t, stops.Load(), "the stop began before the handler returned its answer")
}

func TestAStopRequestIsRefusedWhenItIsNotAPost(t *testing.T) {
	var stops atomic.Int32
	l := &localRequests{log: zerolog.Nop(), stop: func() { stops.Add(1) }}

	for _, method := range []string{"GET", "DELETE", "PUT", "PATCH"} {
		resp := l.Do(context.Background(), ipc.Request{Method: method, Path: ipc.PathStop})
		require.NotNil(t, resp.Error, method)
		assert.Equal(t, ipc.RelayBadRequest, resp.Error.Code, method)
	}
	time.Sleep(2 * stopGrace)
	assert.Zero(t, stops.Load(), "a refused request stopped the daemon")
}

// A path that only begins like the stop request is not one: it falls through to whatever answers the
// rest of the daemon's own paths, and stops nothing.
func TestOnlyTheExactPathStopsTheDaemon(t *testing.T) {
	var stops atomic.Int32
	l := &localRequests{log: zerolog.Nop(), stop: func() { stops.Add(1) }}

	for _, path := range []string{ipc.PathStop + "/", ipc.PathStop + "/now", ipc.PathStop + "?x=1", "/@daemon/STOP"} {
		resp := l.Do(context.Background(), ipc.Request{Method: "POST", Path: path})
		require.NotNil(t, resp.Error, path)
	}
	time.Sleep(2 * stopGrace)
	assert.Zero(t, stops.Load())
}
