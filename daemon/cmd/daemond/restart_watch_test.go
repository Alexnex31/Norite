// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"testing"
	"time"
)

// starter is a program that ends as scripted, one start after another; a nil entry never ends.
type starter struct {
	ends   []func() error
	starts int
}

func (s *starter) start() (func() error, error) {
	end := s.ends[s.starts]
	s.starts++
	if end == nil {
		return func() error { select {} }, nil
	}
	return end, nil
}

func TestAStartedVersionIsWatchedAndStartedAgainWhenItFails(t *testing.T) {
	crash := func() error { return errors.New("exit status 2") }
	clean := func() error { return nil }

	t.Run("one that stays up is started once", func(t *testing.T) {
		s := &starter{ends: []func() error{nil}}
		if err := startAndWatch(s.start, 20*time.Millisecond, 2); err != nil || s.starts != 1 {
			t.Errorf("err %v after %d starts", err, s.starts)
		}
	})
	t.Run("one that crashes is started again, which is the start that undoes it", func(t *testing.T) {
		s := &starter{ends: []func() error{crash, clean}}
		if err := startAndWatch(s.start, time.Minute, 2); err != nil || s.starts != 2 {
			t.Errorf("err %v after %d starts", err, s.starts)
		}
	})
	t.Run("one told to stop is not started again", func(t *testing.T) {
		s := &starter{ends: []func() error{clean, crash}}
		if err := startAndWatch(s.start, time.Minute, 2); err != nil || s.starts != 1 {
			t.Errorf("err %v after %d starts", err, s.starts)
		}
	})
	t.Run("one that never stays up is given up on", func(t *testing.T) {
		s := &starter{ends: []func() error{crash, crash, crash, crash}}
		if err := startAndWatch(s.start, time.Minute, 2); err == nil || s.starts != 3 {
			t.Errorf("err %v after %d starts", err, s.starts)
		}
	})
	t.Run("a program that cannot be started is an error at once", func(t *testing.T) {
		calls := 0
		err := startAndWatch(func() (func() error, error) { calls++; return nil, errors.New("not found") }, time.Minute, 2)
		if err == nil || calls != 1 {
			t.Errorf("err %v after %d calls", err, calls)
		}
	})
}
