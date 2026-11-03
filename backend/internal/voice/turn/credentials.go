// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha1" // TURN's long-term credential mechanism is HMAC-SHA1 (security ledger).
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// CredentialLifetime is how long a credential may start an allocation. It covers a client's gather, not
// its call: the expiry is asked of an Allocate alone, so an allocation outlives its credential for as long
// as it is refreshed (auth.go).
const CredentialLifetime = 10 * time.Minute

// Realm is the realm every credential is for. A constant: it enters the key, and nothing distinguishes
// one instance's realm from another's that the secret does not already.
const Realm = "norite"

// secretInfo is the HKDF info string, distinct from every other derivation from the signing key.
const secretInfo = "norite/turn-secret/v1"

// maxUsername bounds what is parsed of a username a stranger sends: an expiry and a snowflake, both
// decimal, and the colon between them.
const maxUsername = 64

// DeriveSecret is the shared secret credentials are signed with, from the instance signing key. Derived
// rather than configured, so a self-hoster has nothing more to keep; at M116 the TURN pods are given this
// value and never the signing key. Rotating the signing key invalidates every credential, which lives ten
// minutes anyway.
func DeriveSecret(signingKey []byte) ([]byte, error) {
	// Salt is nil, as for the TOTP key: the input is the signing key, which config validation requires
	// to be long, and the info string is what separates the derivations.
	secret, err := hkdf.Key(sha256.New, signingKey, nil, secretInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("turn: deriving the secret: %w", err)
	}
	return secret, nil
}

// Credential is what a client presents to TURN: the REST shape, a username naming its expiry and its
// account, and a password only this instance's secret makes.
type Credential struct {
	Username string
	Password string
	Expires  time.Time
}

// Mint makes a credential for user, valid from now for CredentialLifetime.
//
// It is scoped to the account, not to a voice connection. The client gathers its relayed candidate before
// it makes the offer, and the connection id is minted when the offer arrives, so a credential naming the
// connection could not exist when it is needed. Nothing is lost: the relay reaches the SFU's media port
// and nothing else, a port anyone on the internet can send to already, and the SFU admits nothing it was
// not told to expect. The account is what quotas count.
func Mint(secret []byte, user snowflake.ID, now time.Time) Credential {
	expires := now.Add(CredentialLifetime).Truncate(time.Second)
	username := strconv.FormatInt(expires.Unix(), 10) + ":" + user.String()
	return Credential{Username: username, Password: password(secret, username), Expires: expires}
}

// password is base64(HMAC-SHA1(secret, username)), the TURN REST API's.
func password(secret []byte, username string) string {
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// parseUsername reads a username Mint made, refusing anything else without allocating much for it: a
// stranger chooses this string.
func parseUsername(username string) (expires time.Time, user snowflake.ID, ok bool) {
	if len(username) > maxUsername {
		return time.Time{}, 0, false
	}
	exp, id, found := strings.Cut(username, ":")
	if !found {
		return time.Time{}, 0, false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || unix <= 0 || strconv.FormatInt(unix, 10) != exp {
		return time.Time{}, 0, false
	}
	user, err = snowflake.Parse(id)
	if err != nil || user == 0 || user.String() != id {
		return time.Time{}, 0, false
	}
	return time.Unix(unix, 0), user, true
}
