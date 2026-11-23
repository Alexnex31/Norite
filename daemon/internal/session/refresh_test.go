// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
)

// An instance that cannot be reached is said with the network's own words, and never with the request's
// URL or body, which carries the refresh token (M34's Mac pass: the log said only "could not reach the
// instance", where the CLI on the same Mac reached it).
func TestAnUnreachableInstanceSaysWhyAndNothingOfTheRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens there now
	const token = "nrr_the-refresh-token-itself"
	_, _, err = refreshSession(context.Background(), http.DefaultClient, "http://"+addr, token)
	if err == nil {
		t.Fatal("a refresh reached an address nothing listens on")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "could not reach the instance: ") || !strings.Contains(msg, "connection refused") {
		t.Errorf("the network's reason is missing: %q", msg)
	}
	for _, leak := range []string{token, "/api/v1/auth/refresh", "Post"} {
		if strings.Contains(msg, leak) {
			t.Errorf("the error carries %q: %q", leak, msg)
		}
	}
}
