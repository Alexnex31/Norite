// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	internalstate "github.com/Alexnex31/Norite/daemon/internal/statefile"
	"github.com/Alexnex31/Norite/daemon/internal/voiceworker"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
)

func aCall(alive time.Time) *statefile.VoiceCall {
	return &statefile.VoiceCall{Instance: "https://chat.example", Account: "42", Channel: "1001", Alive: alive,
		Processing: ipc.VoiceAllOn(), Input: ipc.DefaultVoiceInput(), Self: ipc.VoiceSelf{Mute: true},
		LocalMutes: []string{}}
}

// The call to join again is state.json's, beside what else it holds, and removing it removes it alone (M36).
func TestTheCallToJoinAgainIsKeptInTheStateFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, internalstate.Update(context.Background(), dir, func(s *statefile.State) error {
		s.ConfigSplit = true
		return nil
	}, nil))
	b := breadcrumbs{stateDir: dir}
	want := aCall(time.Now().UTC())
	require.NoError(t, b.Save(want))
	st, err := statefile.ReadIn(dir)
	require.NoError(t, err)
	require.True(t, st.ConfigSplit)
	require.NotNil(t, st.VoiceCall)
	require.True(t, st.VoiceCall.Equal(want), "%+v", st.VoiceCall)

	require.NoError(t, b.Save(nil))
	st, err = statefile.ReadIn(dir)
	require.NoError(t, err)
	require.Nil(t, st.VoiceCall)
	require.True(t, st.ConfigSplit)
}

// A breadcrumb the previous run left that is too old to join, or that the daemon did not write, is removed
// at start, and nothing is joined.
func TestAStartRemovesACallItWillNotJoin(t *testing.T) {
	for name, body := range map[string]func(dir string){
		"too old": func(dir string) {
			require.NoError(t, breadcrumbs{stateDir: dir}.Save(aCall(time.Now().Add(-time.Hour))))
		},
		"not the daemon's": func(dir string) {
			require.NoError(t, os.WriteFile(statefile.PathIn(dir),
				[]byte(`{"version":1,"voice_call":{"channel":"general"}}`), 0o600))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			body(dir)
			calls := voiceworker.NewVoice(voiceworker.VoiceOptions{Signaler: voiceworker.Harness{URL: "http://127.0.0.1:1"},
				Account: func() (voiceworker.SignIn, error) {
					return voiceworker.SignIn{Account: "42", Instance: "https://chat.example"}, nil
				},
				Breadcrumbs: breadcrumbs{stateDir: dir}, Log: zerolog.Nop()})
			t.Cleanup(calls.Close)
			resumeCall(dir, calls, zerolog.Nop())
			require.Equal(t, ipc.VoiceIdle, calls.Status().State)
			require.False(t, calls.Busy())
			data, err := os.ReadFile(statefile.PathIn(dir))
			require.NoError(t, err)
			require.NotContains(t, string(data), statefile.VoiceCallField)
		})
	}
}
