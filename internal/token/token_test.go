package token

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)

func signer() *Signer { return NewSigner(bytes.Repeat([]byte{7}, 32)) }

// Telegram /start parameters: up to 64 characters of A-Z a-z 0-9 _ -.
var startParam = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func TestTokenFitsTelegramStartParameter(t *testing.T) {
	s := signer()
	for i := 0; i < 1000; i++ {
		tok, _ := s.Issue(uint16(i), t0)
		if !startParam.MatchString(tok) {
			t.Fatalf("token %q is not a valid Telegram start parameter", tok)
		}
		if !IsCheckin(tok) || IsEnrollCode(tok) {
			t.Fatalf("token %q misclassified", tok)
		}
	}
	for i := 0; i < 1000; i++ {
		c := NewEnrollCode()
		if !startParam.MatchString(c) || !IsEnrollCode(c) || IsCheckin(c) {
			t.Fatalf("enroll code %q invalid or misclassified", c)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	s := signer()
	tok, c := s.Issue(17, t0.Add(400*time.Millisecond))
	got, err := s.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostID != 17 || !got.Issued.Equal(t0) || got.Nonce != c.Nonce {
		t.Fatalf("got %+v, want host 17 issued %v nonce %x", got, t0, c.Nonce)
	}
}

// Expiry table: token issued at 10:00:00 with a 60 s TTL.
func TestTokenExpiry(t *testing.T) {
	ttl := 60 * time.Second
	cases := []struct {
		scanAt time.Time
		want   error
	}{
		{t0, nil},
		{t0.Add(30 * time.Second), nil},
		{t0.Add(60 * time.Second), nil}, // exactly at the limit: accepted
		{t0.Add(60*time.Second + time.Millisecond), ErrExpired},
		{t0.Add(2*time.Minute + time.Second), ErrExpired}, // acceptance: photo > 2 min always rejected
		{t0.Add(24 * time.Hour), ErrExpired},
		{t0.Add(-3 * time.Second), nil}, // within the 5 s rounding skew
		{t0.Add(-10 * time.Second), ErrFuture},
	}
	c := Claims{HostID: 1, Issued: t0}
	for _, tc := range cases {
		if err := c.Check(tc.scanAt, ttl); !errors.Is(err, tc.want) {
			t.Errorf("scan at %v: got %v, want %v", tc.scanAt.Sub(t0), err, tc.want)
		}
	}
}

func TestTokenTampering(t *testing.T) {
	s := signer()
	tok, _ := s.Issue(17, t0)
	raw, _ := b64.DecodeString(tok[1:])

	// Token "from another host": change the host id bytes, keep the MAC.
	forged := append([]byte(nil), raw...)
	forged[2] ^= 0x01
	if _, err := s.Parse("c" + b64.EncodeToString(forged)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("host id changed: got %v, want ErrBadSignature", err)
	}
	// Extend the lifetime by moving the issue time.
	forged = append([]byte(nil), raw...)
	forged[6]++
	if _, err := s.Parse("c" + b64.EncodeToString(forged)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("issue time changed: got %v, want ErrBadSignature", err)
	}
	// Signed with another key.
	other := NewSigner(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Parse(tok); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other key: got %v, want ErrBadSignature", err)
	}
	for _, bad := range []string{"", "c", "x" + tok[1:], tok[:len(tok)-1], tok + "A", strings.Repeat("c", 35), "c" + strings.Repeat("!", 34)} {
		if _, err := s.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func FuzzParse(f *testing.F) {
	s := signer()
	tok, _ := s.Issue(3, t0)
	f.Add(tok)
	f.Add("")
	f.Add("c" + strings.Repeat("A", 34))
	f.Add(NewEnrollCode())
	f.Fuzz(func(t *testing.T, in string) {
		c, err := s.Parse(in)
		if err != nil {
			return
		}
		// Anything that parses must re-sign to exactly the same string:
		// there is only one valid encoding per claim.
		if again := s.Sign(c); again != in {
			t.Fatalf("parsed %q but re-signs to %q", in, again)
		}
	})
}
