// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watching the store, so a login or a logout reaches a running daemon when it happens.
//
// A login reaches it without this — the login supersedes the device's sign-in, the gateway closes with
// 4011, and Revoked reads the store — but a logout revokes nothing at the instance (its help says so), so
// until this the daemon went on streaming the account's messages until its next renewal found no record,
// up to fifteen minutes later. A signed-out daemon, which holds no connection to be closed, learned of a
// new login only at restart.
//
// What is watched is the record (account.json): a login writes it and a logout removes it. Not the secret,
// which every renewal rewrites when the store is the file backend — and Reload of an unchanged store costs
// nothing anyway, so an event this filter lets through by mistake is harmless. The directory is watched
// rather than the file, because the record is replaced by an atomic rename and a watch on the old file
// would follow it into oblivion.

// watchSettle coalesces a burst of events into one reload: an atomic write is a create and a rename, and a
// login writes the record and then the device file.
const watchSettle = 100 * time.Millisecond

// Watch asks Run to reload whenever the record changes, until ctx is done. It returns an error only when no
// watch could be set up — the caller logs that, and the daemon carries on as it did before M19 had this:
// a logout is noticed at the next renewal, a login at the next restart.
func (s *Source) Watch(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	record := s.store.RecordPath()
	if err := w.Add(filepath.Dir(record)); err != nil {
		return err
	}
	name := filepath.Base(record)

	var settle <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if filepath.Base(ev.Name) == name && settle == nil {
				settle = time.After(watchSettle)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			// An overflowed queue means events were dropped, and one of them may have been the record: reload
			// rather than guess. Reload of an unchanged store costs nothing.
			s.log.Warn().Err(err).Msg("the credential watch reported an error; reading the store again")
			s.Reload()
		case <-settle:
			settle = nil
			s.Reload()
		}
	}
}
