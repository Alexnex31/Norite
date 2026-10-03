// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadWithEnv(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()
	withoutConfigFile(t)
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)
	t.Setenv(envPrefix+"JWT_SECRET", testJWTSecret)
	for k, v := range env {
		t.Setenv(envPrefix+k, v)
	}
	return Load("")
}

// TestAnInstanceThatRaisedItsOwnedCeilingStillStarts is the upgrade /code-review found broken at M20a: the
// joined ceiling arrived with a default of 100 and a rule that it be at least the owned one, so an instance
// that had raised guilds_per_account to 200 refused to start. Unset, the joined ceiling now follows the
// owned one, up to what a daemon keeps.
func TestAnInstanceThatRaisedItsOwnedCeilingStillStarts(t *testing.T) {
	for _, tc := range []struct {
		owned      string
		wantJoined int32
	}{
		{"50", 100},    // the defaults
		{"200", 200},   // raised past the joined default: followed
		{"5000", 1000}, // past what a daemon keeps: capped there
	} {
		t.Run(tc.owned, func(t *testing.T) {
			cfg, err := loadWithEnv(t, map[string]string{"MAX_GUILDS_PER_ACCOUNT": tc.owned})
			require.NoError(t, err)
			assert.Equal(t, tc.wantJoined, cfg.MaxJoinedGuildsPerAccount)
		})
	}

	// Set explicitly below the owned ceiling, it simply binds first.
	cfg, err := loadWithEnv(t, map[string]string{
		"MAX_GUILDS_PER_ACCOUNT": "200", "MAX_JOINED_GUILDS_PER_ACCOUNT": "150",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(150), cfg.MaxJoinedGuildsPerAccount)
}

// TestTheJoinedCeilingStopsAtWhatADaemonKeeps: a daemon keeps 1000 guilds (state.maxGuilds), so a ceiling
// above that lets an account join guilds its own client would drop.
func TestTheJoinedCeilingStopsAtWhatADaemonKeeps(t *testing.T) {
	_, err := loadWithEnv(t, map[string]string{"MAX_JOINED_GUILDS_PER_ACCOUNT": "1001"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NORITE_MAX_JOINED_GUILDS_PER_ACCOUNT")
}

// TestACrossFieldRuleNamesTheOtherSetting: a rule comparing two settings named the other one by its Go
// field, which no operator writes. Found on M20a's joined ceiling, and DB_MIN_CONNS had the same message.
func TestACrossFieldRuleNamesTheOtherSetting(t *testing.T) {
	_, err := loadWithEnv(t, map[string]string{"DB_MAX_CONNS": "4", "DB_MIN_CONNS": "8"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NORITE_DB_MAX_CONNS")
	assert.NotContains(t, err.Error(), "(DBMaxConns)")
}
