// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// authorize is the package-local shorthand for [guildauth.Authorize] against this service's pool.
//
// The reasoning it used to carry — ADR 0008's layer separation, and the 404/403 anti-enumeration split —
// moved to guildauth with the function that implements it at M15. It is documented there because four
// packages now read that godoc as their only statement of the refusal contract; leaving it here would
// have kept it where only this package could see it.
//
// channelID may be zero for a guild-level check.
func (s *Service) authorize(
	ctx context.Context,
	actor auth.Actor,
	guildID, channelID snowflake.ID,
	need roles.Permission,
) error {
	_, err := guildauth.Authorize(ctx, s.queries, actor, guildID, channelID, need)
	return err
}
