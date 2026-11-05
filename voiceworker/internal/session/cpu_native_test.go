// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux && norite_opusnative && cgo

package session

import (
	"context"

	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

func init() {
	nativeCodec = func(context.Context) (opus.Codec, error) { return opus.NewNative(), nil }
}
