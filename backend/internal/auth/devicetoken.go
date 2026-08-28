package auth

import (
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The two signed continuations the verification page carries its state in.
//
// # Why signed tokens rather than a session
//
// This backend has no browser sessions and no cookies at all — ADR 0011 retired them for every client that
// exists today, and the BFF layer that brings them back is Phase O. So the page cannot say "you are
// already signed in", and there is nothing for it to hang three steps of state on.
//
// A signed token is the answer this package already reached for once, at M6's username form, and it works
// for the same reasons: nothing here needs revoking, and single-use falls out of the query these tokens
// eventually authorize rather than needing a row to enforce it. ApproveDeviceCode's WHERE clause refuses a
// second approval, so a replayed approval token buys nothing.
//
// # Why two of them rather than one
//
// They assert different things. The first says a browser has entered a code that was live at the time; the
// second says the same browser has also proved who it is. Collapsing them into one token with an optional
// user field would mean a value that authorizes before authentication has happened, and the only thing
// standing between the two would be the handler remembering to look.

// The `typ` claim values. Distinct from each other and from every other token this package signs.
//
// This is the same mechanism that keeps an access token from being spendable as a signup (see
// oauthSignupTokenType). Here it is not the last line of defense — nothing else this package signs carries
// a `dvc` claim, so the shape check refuses them too — but it is the one that states the intent, and the
// one that still holds when a future token type happens to carry a similar shape.
const (
	deviceEntryTokenType    = "device_entry"
	deviceFactorTokenType   = "device_factor"
	deviceApprovalTokenType = "device_approval"
)

// deviceTokenTTL is how long either continuation is good for.
//
// Ten minutes: long enough for a provider round trip with a second factor in it, short enough that a page
// left open on a shared screen stops being useful. Bounded independently by the device code's own life,
// which is what actually ends the authorization.
const deviceTokenTTL = 10 * time.Minute

// deviceClaims is what both continuations carry.
type deviceClaims struct {
	jwt.RegisteredClaims

	TokenType string `json:"typ"`
	// DeviceCodeID is the authorization this browser is working on. The row id rather than either code:
	// the device code never reaches the browser at all, and the user code is what a person types, so
	// putting it back into a value the browser holds would give a page something to replay.
	DeviceCodeID string `json:"dvc"`
	// UserCode is carried only so the approval page can show it back, for comparison against what the
	// terminal is displaying. Never used to look anything up.
	UserCode string `json:"uc"`
	// The account, on an approval token only. It travels in the registered `sub` claim.
}

// deviceContinuation is what a valid continuation carries, once parsed.
type deviceContinuation struct {
	DeviceCodeID int64
	UserCode     string
	// UserID is set on an approval token and zero on an entry token, which is the difference between
	// "this browser knows a code" and "this browser is somebody".
	UserID int64
}

// issueDeviceEntryToken records that a browser has entered a live user code.
func (s *Service) issueDeviceEntryToken(deviceCodeID int64, userCode string) (string, error) {
	return s.signDeviceToken(deviceEntryTokenType, deviceCodeID, userCode, 0)
}

// issueDeviceFactorToken records that a browser has proved a password but still owes a second factor.
//
// M11a's addition, and it is the same argument that produced two tokens rather than one. An entry token
// says a browser knows a live code; an approval token says it has proved whose account this is. On an
// account with a second factor, a browser that has typed a correct password is at neither point — it knows
// more than an entry token asserts and less than an approval token does, and handing it the latter would
// authorize before authentication had finished. So it gets its own type, and /device/2fa is the only
// handler that accepts one.
func (s *Service) issueDeviceFactorToken(deviceCodeID int64, userCode string, userID int64,
) (string, error) {
	return s.signDeviceToken(deviceFactorTokenType, deviceCodeID, userCode, userID)
}

// issueDeviceApprovalToken records that the same browser has since proved who it is.
//
// Minted at exactly one point in each sign-in branch — after a password is verified, and after a provider
// callback resolves to an account — so the set of ways to obtain one is the set of ways to authenticate.
// It takes a factorProof, and that is where the device flow's second factor is enforced. This token means
// "this browser has finished proving who it is", and on an account with a factor that is not true until
// the factor has been proved — so the token cannot be minted without one. Approval itself is a later
// request that can only present a token this function produced.
func (s *Service) issueDeviceApprovalToken(deviceCodeID int64, userCode string, userID int64,
	proof factorProof,
) (string, error) {
	if !proof.authorizes(userID) {
		return "", ErrTwoFactorRequired
	}
	return s.signDeviceToken(deviceApprovalTokenType, deviceCodeID, userCode, userID)
}

func (s *Service) signDeviceToken(typ string, deviceCodeID int64, userCode string, userID int64,
) (string, error) {
	now := s.now()

	subject := ""
	if userID != 0 {
		subject = strconv.FormatInt(userID, 10)
	}

	signed, err := s.issuer.sign(deviceClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(deviceTokenTTL)),
			ID:        newJTI(),
		},
		TokenType:    typ,
		DeviceCodeID: strconv.FormatInt(deviceCodeID, 10),
		UserCode:     userCode,
	})
	if err != nil {
		return "", fmt.Errorf("signing device continuation: %w", err)
	}
	return signed, nil
}

// deviceTokenNamesAnAccount reports whether a continuation of this type carries a subject.
//
// M11a added the second one, and forgetting it was a real bug rather than a hypothetical: the factor token
// was minted with a subject and parsed without, so the code form's user id came back as zero and no code
// could ever pass. Caught by the device-flow test, which is exactly the path that would otherwise have
// shipped a second factor nobody could get past.
func deviceTokenNamesAnAccount(typ string) bool {
	return typ == deviceApprovalTokenType || typ == deviceFactorTokenType
}

// parseDeviceToken validates a continuation of exactly the type asked for.
//
// The type is a parameter rather than something the caller checks afterwards, because "afterwards" is
// where that check gets forgotten. Asking for an approval token and being handed an entry token is the
// one confusion that matters here — it is the difference between a browser that knows a code and a
// browser that has proved whose account it is signing in.
func (s *Service) parseDeviceToken(raw, want string) (deviceContinuation, error) {
	var claims deviceClaims

	_, err := jwt.ParseWithClaims(raw, &claims, s.issuer.keyFunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.issuer.now),
	)
	if err != nil {
		return deviceContinuation{}, ErrDeviceContinuation
	}
	if claims.TokenType != want {
		return deviceContinuation{}, ErrDeviceContinuation
	}

	deviceCodeID, err := strconv.ParseInt(claims.DeviceCodeID, 10, 64)
	if err != nil || deviceCodeID == 0 {
		return deviceContinuation{}, ErrDeviceContinuation
	}

	// Re-validated rather than trusted, as the signup token re-validates its own redirect and challenge:
	// this value is displayed, and a future caller minting one differently is the case a backstop is for.
	if _, err := ParseUserCode(claims.UserCode); err != nil {
		return deviceContinuation{}, ErrDeviceContinuation
	}

	out := deviceContinuation{DeviceCodeID: deviceCodeID, UserCode: claims.UserCode}

	// Two of the three types name an account, and the subject is *required* on those rather than merely
	// read: a factor or approval token without one would be a continuation asserting that some browser had
	// authenticated as nobody in particular.
	//
	// An entry token deliberately carries none. That asymmetry is the whole reason these are separate
	// types — see this file's header — and it is why the check is on the type asked for rather than on
	// whether a subject happens to be present.
	if deviceTokenNamesAnAccount(want) {
		userID, err := strconv.ParseInt(claims.Subject, 10, 64)
		if err != nil || userID == 0 {
			return deviceContinuation{}, ErrDeviceContinuation
		}
		out.UserID = userID
	}
	return out, nil
}
