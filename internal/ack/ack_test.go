package ack_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Sarthak-47/kerberon/internal/ack"
)

const secret = "0123456789abcdef0123456789abcdef"

func signer(t *testing.T) *ack.Signer {
	t.Helper()
	s, err := ack.NewSigner(secret)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return s
}

// A short key makes tokens guessable, and this key is the only thing between
// an attacker and silencing a page.
func TestShortSecretIsRejected(t *testing.T) {
	for _, s := range []string{"", "short", strings.Repeat("x", ack.MinSecretLength-1)} {
		if _, err := ack.NewSigner(s); err == nil {
			t.Errorf("secret of length %d should be rejected", len(s))
		}
	}
	if _, err := ack.NewSigner(strings.Repeat("x", ack.MinSecretLength)); err != nil {
		t.Errorf("a secret of the minimum length should be accepted: %v", err)
	}
}

func TestTokenVerifies(t *testing.T) {
	s := signer(t)
	tok := s.Token(42, "sarthak", 1)

	if !s.Verify(42, "sarthak", 1, tok) {
		t.Fatal("a freshly minted token did not verify")
	}
	if tok == "" {
		t.Fatal("empty token")
	}
}

func TestTokenIsDeterministic(t *testing.T) {
	s := signer(t)
	if s.Token(42, "sarthak", 1) != s.Token(42, "sarthak", 1) {
		t.Error("the same triple produced different tokens; a link would stop working")
	}
}

// Every signed field must actually be bound in, or a token becomes
// transferable to somewhere it should not work.
func TestTokenIsBoundToEveryField(t *testing.T) {
	s := signer(t)
	tok := s.Token(42, "sarthak", 1)

	cases := []struct {
		name      string
		incident  int64
		user      string
		step      int
		reasonWhy string
	}{
		{"another incident", 43, "sarthak", 1,
			"a leaked link must not acknowledge a different incident"},
		{"another user", 42, "priya", 1,
			"one recipient of a page must not be able to acknowledge as another"},
		{"another step", 42, "sarthak", 2,
			"a link from an earlier step must not work once escalation has moved on"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if s.Verify(c.incident, c.user, c.step, tok) {
				t.Errorf("token verified for %s: %s", c.name, c.reasonWhy)
			}
		})
	}
}

func TestForgedAndMangledTokensAreRejected(t *testing.T) {
	s := signer(t)
	valid := s.Token(42, "sarthak", 1)

	bad := []string{
		"",
		"not-a-token",
		valid[:len(valid)-1],     // truncated
		valid + "x",              // extended
		strings.ToUpper(valid),   // case-mangled
		"AAAAAAAAAAAAAAAAAAAAAA", // right shape, wrong bytes
	}
	for _, tok := range bad {
		if s.Verify(42, "sarthak", 1, tok) {
			t.Errorf("token %q was accepted", tok)
		}
	}
}

func TestDifferentSecretsProduceDifferentTokens(t *testing.T) {
	a := signer(t)
	b, err := ack.NewSigner("fedcba9876543210fedcba9876543210")
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}

	tok := a.Token(42, "sarthak", 1)
	if b.Verify(42, "sarthak", 1, tok) {
		t.Error("a token signed with one secret verified under another")
	}
}

// Length-prefixing the signed fields is what stops two distinct triples
// signing identically. With a plain separator, a user id containing that
// separator could be split a different way and collide — the same class of bug
// as concatenating labels without a boundary when fingerprinting.
func TestUserIdsCannotCollideAcrossFieldBoundaries(t *testing.T) {
	s := signer(t)

	// If fields were joined without length prefixes, these two could produce
	// the same signed message.
	first := s.Token(1, "ab", 2)
	second := s.Token(1, "a", 2)
	if first == second {
		t.Error("distinct user ids produced the same token")
	}

	// A user id containing the characters a naive encoder might delimit on.
	for _, weird := range []string{"a|b", "a:b", "a/b", "a\x00b", "12"} {
		tok := s.Token(1, weird, 0)
		if !s.Verify(1, weird, 0, tok) {
			t.Errorf("token for user %q did not verify", weird)
		}
		if s.Verify(1, "other", 0, tok) {
			t.Errorf("token for user %q verified for a different user", weird)
		}
	}
}

// ─── Links ────────────────────────────────────────────────────────────────

func TestLinkRoundTrips(t *testing.T) {
	s := signer(t)
	link := s.Link("https://kerberon.example.com", 42, "sarthak", 1)

	if !strings.HasPrefix(link, "https://kerberon.example.com/ack/42/sarthak/") {
		t.Fatalf("unexpected link shape: %s", link)
	}

	path := strings.TrimPrefix(link, "https://kerberon.example.com")
	parsed, err := ack.ParseLinkPath(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.IncidentID != 42 || parsed.UserID != "sarthak" {
		t.Fatalf("parsed = %+v", parsed)
	}
	if !s.Verify(parsed.IncidentID, parsed.UserID, 1, parsed.Token) {
		t.Error("the token from a round-tripped link did not verify")
	}
}

func TestLinkToleratesATrailingSlashOnTheBase(t *testing.T) {
	s := signer(t)
	with := s.Link("https://k.example.com/", 42, "sarthak", 1)
	without := s.Link("https://k.example.com", 42, "sarthak", 1)
	if with != without {
		t.Errorf("a trailing slash changed the link:\n  %s\n  %s", with, without)
	}
}

// A user id with characters that need escaping must survive the URL.
func TestLinkEscapesTheUserId(t *testing.T) {
	s := signer(t)
	const user = "team/lead onboarding"

	link := s.Link("https://k.example.com", 7, user, 0)
	if strings.Contains(link, " ") {
		t.Errorf("link contains a raw space: %s", link)
	}

	path := strings.TrimPrefix(link, "https://k.example.com")
	parsed, err := ack.ParseLinkPath(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.UserID != user {
		t.Errorf("user id round-tripped as %q, want %q", parsed.UserID, user)
	}
	if !s.Verify(parsed.IncidentID, parsed.UserID, 0, parsed.Token) {
		t.Error("token did not verify after escaping")
	}
}

// A mangled link is usually a truncated notification; a forged one is an
// attack. The caller needs to tell them apart.
func TestMalformedLinksAreDistinguishedFromForgedOnes(t *testing.T) {
	bad := []string{
		"", "/", "/ack", "/ack/42", "/ack/42/sarthak",
		"/nope/42/sarthak/token",
		"/ack/abc/sarthak/token",
		"/ack/0/sarthak/token",
		"/ack/-1/sarthak/token",
		"/ack/42//token",
		"/ack/42/sarthak/",
	}
	for _, p := range bad {
		if _, err := ack.ParseLinkPath(p); !errors.Is(err, ack.ErrMalformedLink) {
			t.Errorf("path %q: err = %v, want ErrMalformedLink", p, err)
		}
	}

	// Well-formed but forged: parses fine, fails verification.
	parsed, err := ack.ParseLinkPath("/ack/42/sarthak/AAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatalf("a well-formed link should parse: %v", err)
	}
	if signer(t).Verify(parsed.IncidentID, parsed.UserID, 0, parsed.Token) {
		t.Error("a forged token verified")
	}
}
