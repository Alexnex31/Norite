// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package voice is the Pion-based SFU (docs/architecture.md §6, ADRs 0012 and 0023, M26): rooms of
// connections, each admitted by the DTLS fingerprint in the offer the backend relays, sending its tracks
// and receiving others' over pre-allocated slots.
//
// Nothing here starts on its own: norite-server starts the SFU at M33, which also builds the backend's
// half of admission over Control. Until then the tests and voicetest's harness are its only callers.
//
// No bearer token reaches this package (ADR 0023). What admits a connection is the backend's Provision
// with the client's offer, and DTLS refusing any certificate that is not the offer's fingerprint.
package voice
