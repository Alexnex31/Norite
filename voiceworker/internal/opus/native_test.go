// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build norite_opusnative && cgo

package opus

func init() { native = NewNative }
