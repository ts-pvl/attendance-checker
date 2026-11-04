// Package token creates and verifies the short-lived QR check-in tokens and
// the one-time enrollment codes.
//
// A check-in token is "c" + base64url(version | host id | issued | nonce | mac)
// where mac is HMAC-SHA256 over the preceding bytes, truncated to 12 bytes.
// The result is 35 characters, inside Telegram's 64-character limit for
// /start parameters and its [A-Za-z0-9_-] alphabet.
package token

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const (
	version    = 1
	nonceLen   = 6
	macLen     = 12
	payloadLen = 1 + 2 + 4 + nonceLen // version, host id, issued (unix s), nonce
	rawLen     = payloadLen + macLen

	checkinPrefix = "c"
	enrollPrefix  = "e"

	// MaxFutureSkew tolerates a token "issued" slightly after now. Both times
	// come from the server clock, so this only absorbs second rounding.
	MaxFutureSkew = 5 * time.Second
)

var (
	ErrMalformed    = errors.New("token: malformed")
	ErrBadSignature = errors.New("token: bad signature")
	ErrExpired      = errors.New("token: expired")
	ErrFuture       = errors.New("token: issued in the future")
)

// Strict rejects non-zero trailing bits, so every token has exactly one encoding.
var b64 = base64.RawURLEncoding.Strict()

// Claims is the content of a check-in token.
type Claims struct {
	HostID uint16
	Issued time.Time // second precision
	Nonce  [nonceLen]byte
}

// NonceString identifies the token for single-use tracking.
func (c Claims) NonceString() string {
	return hex.EncodeToString(c.Nonce[:])
}

// Check validates the token's age against ttl at time now.
func (c Claims) Check(now time.Time, ttl time.Duration) error {
	age := now.Sub(c.Issued)
	if age < -MaxFutureSkew {
		return ErrFuture
	}
	if age > ttl {
		return ErrExpired
	}
	return nil
}

type Signer struct {
	key []byte
}

func NewSigner(key []byte) *Signer {
	if len(key) < 16 {
		panic("token: HMAC key too short")
	}
	return &Signer{key: append([]byte(nil), key...)}
}

// Issue creates a token for hostID with a fresh random nonce.
func (s *Signer) Issue(hostID uint16, now time.Time) (string, Claims) {
	c := Claims{HostID: hostID, Issued: now.Truncate(time.Second)}
	if _, err := rand.Read(c.Nonce[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return s.Sign(c), c
}

func (s *Signer) Sign(c Claims) string {
	var raw [rawLen]byte
	raw[0] = version
	binary.BigEndian.PutUint16(raw[1:3], c.HostID)
	binary.BigEndian.PutUint32(raw[3:7], uint32(c.Issued.Unix()))
	copy(raw[7:payloadLen], c.Nonce[:])
	copy(raw[payloadLen:], s.mac(raw[:payloadLen]))
	return checkinPrefix + b64.EncodeToString(raw[:])
}

// Parse decodes tok and verifies its signature. It does not check expiry.
func (s *Signer) Parse(tok string) (Claims, error) {
	if !IsCheckin(tok) {
		return Claims{}, ErrMalformed
	}
	raw, err := b64.DecodeString(tok[len(checkinPrefix):])
	if err != nil || len(raw) != rawLen || raw[0] != version {
		return Claims{}, ErrMalformed
	}
	if !hmac.Equal(raw[payloadLen:], s.mac(raw[:payloadLen])) {
		return Claims{}, ErrBadSignature
	}
	var c Claims
	c.HostID = binary.BigEndian.Uint16(raw[1:3])
	c.Issued = time.Unix(int64(binary.BigEndian.Uint32(raw[3:7])), 0)
	copy(c.Nonce[:], raw[7:payloadLen])
	return c, nil
}

func (s *Signer) mac(payload []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write(payload)
	return m.Sum(nil)[:macLen]
}

// IsCheckin reports whether a /start parameter looks like a check-in token.
func IsCheckin(s string) bool {
	return len(s) == len(checkinPrefix)+b64.EncodedLen(rawLen) && strings.HasPrefix(s, checkinPrefix)
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewEnrollCode returns a random one-time enrollment code (120 bits).
func NewEnrollCode() string {
	var b [15]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return enrollPrefix + strings.ToLower(b32.EncodeToString(b[:]))
}

// IsEnrollCode reports whether a /start parameter looks like an enrollment code.
func IsEnrollCode(s string) bool {
	if len(s) != 25 || !strings.HasPrefix(s, enrollPrefix) {
		return false
	}
	for _, r := range s[1:] {
		if !(r >= 'a' && r <= 'z' || r >= '2' && r <= '7') {
			return false
		}
	}
	return true
}

// NewSecret returns n random bytes, for HMAC keys and host keys.
func NewSecret(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
