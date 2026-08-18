// Package ack mints and verifies the signed links that let someone
// acknowledge an incident from their phone.
//
// The person being paged is holding a phone at 3am. Requiring them to log in
// first is how incidents end up unacknowledged, so a signed link is the entire
// authentication story for this one action. Its blast radius is deliberately
// tiny: a leaked link acknowledges one incident, for one recipient, at one
// escalation step, and the act is loudly visible in the incident timeline
// (spec section 8.4).
package ack

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// tokenBytes is how much of the HMAC is kept. 128 bits is far beyond forgeable
// while keeping the link short enough to survive an SMS or a notification
// preview.
const tokenBytes = 16

// MinSecretLength is the shortest secret accepted. A short key makes tokens
// guessable, and this key is the only thing standing between an attacker and
// silencing a page.
const MinSecretLength = 16

// Signer mints and verifies ack tokens.
type Signer struct {
	key []byte
}

// NewSigner returns a Signer for the configured secret.
func NewSigner(secret string) (*Signer, error) {
	if len(secret) < MinSecretLength {
		return nil, fmt.Errorf(
			"ack: secret_key is %d bytes; want at least %d. Generate one with: openssl rand -hex 32",
			len(secret), MinSecretLength)
	}
	return &Signer{key: []byte(secret)}, nil
}

// Token signs the (incident, user, step) triple.
//
// All three are bound in: a token is not transferable to another incident, nor
// to another recipient of the same page, nor reusable on a later escalation
// step. That last one matters because each step mints fresh links, and an
// earlier recipient should not be able to acknowledge work that has already
// moved past them.
func (s *Signer) Token(incidentID int64, userID string, step int) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write(canonical(incidentID, userID, step))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:tokenBytes])
}

// Verify reports whether token is valid for the triple. The comparison is
// constant time: a variable-time compare leaks the token a byte at a time to
// anyone who can measure response latency.
func (s *Signer) Verify(incidentID int64, userID string, step int, token string) bool {
	want := s.Token(incidentID, userID, step)
	return subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

// VerifyAny checks a token against every step up to and including maxStep,
// returning which one minted it.
//
// The link deliberately does not carry the step number. Including it would let
// anyone holding one link enumerate the others by editing a digit, and the
// server already knows how far escalation has run, so trying the steps that
// could plausibly have minted the token costs nothing. The search is bounded
// by the incident's own progress rather than by anything the caller supplies.
func (s *Signer) VerifyAny(incidentID int64, userID string, maxStep int, token string) (int, bool) {
	if maxStep < 0 {
		maxStep = 0
	}
	// Cap the work regardless of what the incident claims, so a corrupt
	// current_step cannot turn one request into unbounded hashing.
	const hardLimit = 256
	if maxStep > hardLimit {
		maxStep = hardLimit
	}
	for step := 0; step <= maxStep; step++ {
		if s.Verify(incidentID, userID, step, token) {
			return step, true
		}
	}
	return 0, false
}

// canonical encodes the signed fields unambiguously.
//
// Each variable-length field is length-prefixed rather than delimited. With a
// plain separator, a user id containing that separator could be split
// differently and two distinct triples would sign identically — the same
// class of bug as concatenating labels without a boundary when fingerprinting.
func canonical(incidentID int64, userID string, step int) []byte {
	buf := make([]byte, 0, 8+8+len(userID)+8)
	buf = binary.BigEndian.AppendUint64(buf, uint64(incidentID))
	buf = binary.BigEndian.AppendUint64(buf, uint64(len(userID)))
	buf = append(buf, userID...)
	buf = binary.BigEndian.AppendUint64(buf, uint64(step))
	return buf
}

// Link builds the URL that goes in a notification.
//
// externalURL must be an address the paged person can actually open; a link
// pointing at localhost is useless on a phone, which is why config requires it.
func (s *Signer) Link(externalURL string, incidentID int64, userID string, step int) string {
	base := strings.TrimRight(externalURL, "/")
	return fmt.Sprintf("%s/ack/%d/%s/%s",
		base,
		incidentID,
		url.PathEscape(userID),
		s.Token(incidentID, userID, step),
	)
}

// ErrMalformedLink reports a path that is not an ack link at all, as distinct
// from a well-formed link whose signature does not verify.
var ErrMalformedLink = errors.New("not a valid acknowledgement link")

// Parsed is the content of an ack link.
type Parsed struct {
	IncidentID int64
	UserID     string
	Token      string
}

// ParseLinkPath extracts the fields from "/ack/{incident}/{user}/{token}".
//
// Parsing is separate from verification so a caller can report a mangled link
// differently from a forged one — the first is usually a truncated
// notification, the second is an attack.
func ParseLinkPath(path string) (Parsed, error) {
	trimmed := strings.Trim(path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 4 || parts[0] != "ack" {
		return Parsed{}, ErrMalformedLink
	}

	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return Parsed{}, ErrMalformedLink
	}
	user, err := url.PathUnescape(parts[2])
	if err != nil || user == "" {
		return Parsed{}, ErrMalformedLink
	}
	if parts[3] == "" {
		return Parsed{}, ErrMalformedLink
	}
	return Parsed{IncidentID: id, UserID: user, Token: parts[3]}, nil
}
