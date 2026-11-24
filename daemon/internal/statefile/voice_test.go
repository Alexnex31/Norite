// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package statefile

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
)

// call is a breadcrumb as the daemon writes one, for a muted caller with somebody muted for themselves.
func call() *statefile.VoiceCall {
	return &statefile.VoiceCall{Instance: "https://chat.example", Account: "42", Channel: "1001",
		Alive: time.Date(2026, 11, 24, 10, 0, 0, 123, time.UTC), Attempts: 1,
		Processing: ipc.VoiceJoin{EchoCancellation: true, AutoGain: true},
		Input:      ipc.VoiceInput{Mode: ipc.VoiceInputPushToTalk, Key: "ctrl+f5"},
		Self:       ipc.VoiceSelf{Mute: true}, LocalMutes: []string{"7", "9"}, MaxStreams: 2}
}

func keep(c *statefile.VoiceCall) func(*statefile.State) error {
	return func(s *statefile.State) error { s.VoiceCall = c; return nil }
}

// A breadcrumb is read back as it was written, beside the rest of the state, and removing it leaves no
// field behind.
func TestABreadcrumbIsReadBackAndRemovedWhole(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Update(context.Background(), dir, split(true), nil))
	require.NoError(t, Update(context.Background(), dir, keep(call()), nil))
	s, err := statefile.ReadIn(dir)
	require.NoError(t, err)
	require.NotNil(t, s.VoiceCall)
	require.NoError(t, s.VoiceCall.Refused())
	assert.True(t, s.VoiceCall.Equal(call()), "read back: %+v", s.VoiceCall)
	assert.True(t, s.ConfigSplit)

	require.NoError(t, Update(context.Background(), dir, keep(nil), nil))
	data, err := os.ReadFile(statefile.PathIn(dir))
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"config_split":true,"automation_enabled":false,"automation_port":0,
		"automation_instance":""}`, string(data))
}

// The same breadcrumb written again writes nothing: State is compared by what it holds, never by the
// breadcrumb's address, or the stamp refreshed every few seconds would rewrite the file for nothing besides.
func TestTheSameBreadcrumbWritesNothing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Update(context.Background(), dir, keep(call()), nil))
	require.NoError(t, os.Chmod(statefile.PathIn(dir), 0o400))
	t.Cleanup(func() { _ = os.Chmod(statefile.PathIn(dir), 0o600) })
	before, err := os.Stat(statefile.PathIn(dir))
	require.NoError(t, err)
	require.NoError(t, Update(context.Background(), dir, keep(call()), nil))
	after, err := os.Stat(statefile.PathIn(dir))
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "the file was replaced")

	moved := call()
	moved.Alive = moved.Alive.Add(time.Second)
	require.NoError(t, Update(context.Background(), dir, keep(moved), nil))
	s, err := statefile.ReadIn(dir)
	require.NoError(t, err)
	assert.True(t, s.VoiceCall.Alive.Equal(moved.Alive), "a new stamp is written")
}

// What is read back is a file anybody running as the account can edit (rule 19). Each field held to what the
// daemon writes refuses the whole breadcrumb, and the rest of the file is still read, since every client reads
// it to find its config. The next write drops what was refused.
func TestABreadcrumbTheDaemonDidNotWriteIsRefusedAndTheRestIsRead(t *testing.T) {
	good, err := json.Marshal(call())
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(good, &fields))
	with := func(key, value string) string {
		f := maps.Clone(fields)
		if value == "" {
			delete(f, key)
		} else {
			f[key] = json.RawMessage(value)
		}
		out, err := json.Marshal(f)
		require.NoError(t, err)
		return string(out)
	}
	for name, voice := range map[string]string{
		"not an object":           `[1]`,
		"an unknown field":        with("extra", `1`),
		"a field of another type": with("attempts", `"1"`),
		"no instance":             with("instance", `""`),
		"an instance not http":    with("instance", `"file:///etc/passwd"`),
		"an instance too long":    with("instance", `"https://`+longHost()+`"`),
		"an account not an id":    with("account", `"007"`),
		"a channel not an id":     with("channel", `"general"`),
		"a channel past 64 bits":  with("channel", `"18446744073709551616"`),
		"no stamp":                with("alive", ""),
		"negative attempts":       with("attempts", `-1`),
		"deaf and not muted":      with("self", `{"mute":false,"deaf":true}`),
		"a mute kept undeafened":  with("mute_before_deaf", `true`),
		"too many streams":        with("max_streams", `4`),
		"negative streams":        with("max_streams", `-1`),
		"no local mutes":          with("local_mutes", `null`),
		"a mute not an id":        with("local_mutes", `["7","x"]`),
		"its own account muted":   with("local_mutes", `["42"]`),
		"mutes not sorted":        with("local_mutes", `["9","7"]`),
		"a mute twice":            with("local_mutes", `["7","7"]`),
		"too many mutes":          with("local_mutes", tooMany()),
		"a mode not a word":       with("input", `{"input_mode":"always","push_to_talk_key":"ctrl+f5"}`),
		"a key not canonical":     with("input", `{"input_mode":"voice","push_to_talk_key":"Shift+Ctrl+Space"}`),
		"a key typing holds":      with("input", `{"input_mode":"voice","push_to_talk_key":"a"}`),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			body := `{"version":1,"config_split":true,"voice_call":` + voice + `}`
			require.NoError(t, os.WriteFile(statefile.PathIn(dir), []byte(body), 0o600))
			s, err := statefile.ReadIn(dir)
			require.NoError(t, err, "the rest of the file is read")
			assert.True(t, s.ConfigSplit)
			require.NotNil(t, s.VoiceCall)
			require.Error(t, s.VoiceCall.Refused())
			assert.Empty(t, s.VoiceCall.Instance+s.VoiceCall.Account+s.VoiceCall.Channel,
				"nothing of a refused breadcrumb is kept")

			require.NoError(t, Update(context.Background(), dir, split(false), nil))
			data, err := os.ReadFile(statefile.PathIn(dir))
			require.NoError(t, err)
			assert.NotContains(t, string(data), statefile.VoiceCallField)
		})
	}
	// And the good one, through the same helper, is not refused: the cases above fail for their own field.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(statefile.PathIn(dir),
		[]byte(`{"version":1,"voice_call":`+with("attempts", `0`)+`}`), 0o600))
	s, err := statefile.ReadIn(dir)
	require.NoError(t, err)
	require.NoError(t, s.VoiceCall.Refused())
}

func longHost() string {
	b := make([]byte, ipc.MaxInstanceURL)
	for i := range b {
		b[i] = 'a'
	}
	return string(b) + ".example"
}

// tooMany is one more user than a person may mute, each a user id, sorted and once.
func tooMany() string {
	ids := make([]string, ipc.MaxLocalMutes+1)
	for i := range ids {
		ids[i] = strconv.Itoa(1000 + i)
	}
	out, _ := json.Marshal(ids)
	return string(out)
}
