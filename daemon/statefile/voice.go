// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package statefile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// VoiceCall is the call the daemon was in and did not leave (M36): the "last active voice channel"
// breadcrumb, the one thing a call leaves on disk, so that a daemon that went away mid-call, or whose worker
// or gateway session did, joins it again. It is written while the call is up and removed by every end that
// is not to be rejoined: a leave, an end the instance or the worker chose, the sign-in's end.
//
// It names its instance and account, since a daemon starting signed in to another discards it, and carries
// what the person set (M34) and what the call ran, so that the rejoin is the call as it was: a caller who had
// muted themselves comes back muted. It holds no credential: ids, switches and a key's name.
//
// Read back, it is a file anybody running as the account can edit, so it is foreign again (rule 19): a
// field that is not what the daemon writes leaves it Refused, and the daemon discards it. Refusing it never
// fails reading the rest of the file, which every client reads to find its config.
type VoiceCall struct {
	Instance string `json:"instance"`
	Account  string `json:"account"`
	Channel  string `json:"channel"`
	// Alive is the last moment the call was known up, on the wall clock: a daemon asleep or frozen refreshes
	// nothing, and the rejoin is bounded by how long ago this was.
	Alive time.Time `json:"alive"`
	// Attempts is how many rejoins have been tried since the call was last up long enough, counted before
	// each is made, so that a rejoin which takes the daemon down does not come back for ever.
	Attempts int `json:"attempts"`

	Processing ipc.VoiceJoin  `json:"processing"`
	Input      ipc.VoiceInput `json:"input"`
	// Self is the person's own mute and deafen, and MuteBeforeDeaf the mute an undeafen puts back.
	Self           ipc.VoiceSelf `json:"self"`
	MuteBeforeDeaf bool          `json:"mute_before_deaf"`
	// LocalMutes is whom the person muted for themselves, sorted, and MaxStreams how many they hear at once,
	// 0 for every one the call has room for.
	LocalMutes []string `json:"local_mutes"`
	MaxStreams int      `json:"max_streams"`

	// refused is why what was read is not a breadcrumb the daemon wrote; nil for one it did.
	refused error
}

// Refused says why the breadcrumb read is not one the daemon wrote, or is nil.
func (c *VoiceCall) Refused() error { return c.refused }

// voiceCallFields is VoiceCall without its methods, for encoding/json.
type voiceCallFields VoiceCall

// UnmarshalJSON reads a breadcrumb strictly: every field of its type, none other, and each as Check holds
// it. It never fails, since a breadcrumb refused is discarded and the file around it is still read; what was
// wrong is kept for Refused.
func (c *VoiceCall) UnmarshalJSON(data []byte) error {
	var f voiceCallFields
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err := dec.Decode(&f)
	if err == nil {
		err = (*VoiceCall)(&f).Check()
	}
	if err != nil {
		*c = VoiceCall{refused: fmt.Errorf("the voice call it names is not one Norite wrote: %w", err)}
		return nil
	}
	*c = VoiceCall(f)
	return nil
}

// MarshalJSON writes the breadcrumb, or null for one refused, so that the next write of the file drops it.
func (c *VoiceCall) MarshalJSON() ([]byte, error) {
	if c.refused != nil {
		return []byte("null"), nil
	}
	return json.Marshal((*voiceCallFields)(c))
}

// Check is whether c is a breadcrumb the daemon writes: what a join request would be held to, field by
// field (ipc), and what the daemon's own switches allow.
func (c *VoiceCall) Check() error {
	if err := ipc.CheckInstanceURL(c.Instance); err != nil {
		return err
	}
	if u, err := url.Parse(c.Instance); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("its instance is not an http or https address")
	}
	switch {
	case !ipc.IsUserID(c.Account):
		return errors.New("its account is not a user id")
	case !ipc.IsChannelID(c.Channel):
		return errors.New("its channel is not a channel id")
	case c.Alive.IsZero():
		return errors.New("it says nothing of when the call was up")
	case c.Attempts < 0:
		return errors.New("its count of attempts is negative")
	case c.Self.Deaf && !c.Self.Mute:
		return errors.New("it is deafened and not muted, which a deafen never leaves")
	case c.MuteBeforeDeaf && !c.Self.Deaf:
		return errors.New("it keeps a mute for an undeafen while not deafened")
	case c.MaxStreams < 0 || c.MaxStreams > ipc.MaxStreams:
		return fmt.Errorf("it hears %d people at once, where 0 to %d is", c.MaxStreams, ipc.MaxStreams)
	case len(c.LocalMutes) > ipc.MaxLocalMutes:
		return fmt.Errorf("it mutes more than %d people", ipc.MaxLocalMutes)
	case c.LocalMutes == nil:
		return errors.New("it says nothing of whom the person muted")
	}
	if err := c.Input.Check(); err != nil {
		return err
	}
	for i, u := range c.LocalMutes {
		switch {
		case !ipc.IsUserID(u):
			return errors.New("it mutes something that is not a user id")
		case u == c.Account:
			return errors.New("it mutes its own account")
		case i > 0 && c.LocalMutes[i-1] >= u:
			return errors.New("its muted users are not sorted, each once")
		}
	}
	return nil
}

// Equal is whether c and o are the same breadcrumb, nil being none.
func (c *VoiceCall) Equal(o *VoiceCall) bool {
	if c == nil || o == nil {
		return c == o
	}
	return c.Instance == o.Instance && c.Account == o.Account && c.Channel == o.Channel &&
		c.Alive.Equal(o.Alive) && c.Attempts == o.Attempts && c.Processing == o.Processing &&
		c.Input == o.Input && c.Self == o.Self && c.MuteBeforeDeaf == o.MuteBeforeDeaf &&
		slices.Equal(c.LocalMutes, o.LocalMutes) && c.MaxStreams == o.MaxStreams &&
		(c.refused == nil) == (o.refused == nil)
}
