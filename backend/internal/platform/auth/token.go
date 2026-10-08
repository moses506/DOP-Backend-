package auth

import (
	"context"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type claims struct {
	Name     string `json:"name"`
	Position string `json:"pos"`
	Role     Role   `json:"role"`
	jwt.RegisteredClaims
}

// ErrSessionRevoked is returned by a SessionCheck when a token is valid but the
// account was deactivated, or its password or role changed after the token was issued.
var ErrSessionRevoked = errors.New("session revoked")

// SessionCheck confirms on every request that a signed token still belongs to an
// active account and an open session. It may fill in session details on p.
// It is installed by the users module so auth stays DB-free.
type SessionCheck func(ctx context.Context, p *Principal) error

type Tokens struct {
	secret []byte
	ttl    time.Duration
	check  SessionCheck
	// SecureCookie marks the session cookie HTTPS-only. On in production.
	SecureCookie bool
}

// TTL is the absolute lifetime of a session.
func (t *Tokens) TTL() time.Duration { return t.ttl }

func NewTokens(secret []byte, ttl time.Duration) *Tokens { return &Tokens{secret: secret, ttl: ttl} }

// SetCheck installs the per-request session check.
func (t *Tokens) SetCheck(fn SessionCheck) { t.check = fn }

func (t *Tokens) Issue(p Principal, now time.Time) (string, time.Time, error) {
	exp := now.Add(t.ttl)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Name: p.Name, Position: p.Position, Role: p.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        p.SessionID.String(),
			Subject:   p.UserID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			Issuer:    "dos-bpas",
		},
	})
	s, err := tok.SignedString(t.secret)
	return s, exp, err
}

func (t *Tokens) Parse(raw string) (Principal, error) {
	var c claims
	_, err := jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer("dos-bpas"),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return Principal{}, err
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return Principal{}, errors.New("bad subject")
	}
	if !c.Role.Valid() {
		return Principal{}, errors.New("bad role")
	}
	var iat time.Time
	if c.IssuedAt != nil {
		iat = c.IssuedAt.Time
	}
	sid, _ := uuid.Parse(c.ID) // tokens without a session are rejected by the session check
	return Principal{UserID: id, Name: c.Name, Position: c.Position, Role: c.Role, IssuedAt: iat, SessionID: sid}, nil
}

// A challenge proves the password was right while the second step (the
// authenticator code) is still to come. It lasts five minutes and can't be
// used as a session: it has its own issuer.
const challengeIssuer = "dos-bpas/two-step"

func (t *Tokens) IssueChallenge(userID uuid.UUID, now time.Time) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject: userID.String(), Issuer: challengeIssuer, ID: uuid.NewString(),
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
	}).SignedString(t.secret)
}

func (t *Tokens) ParseChallenge(raw string) (uuid.UUID, error) {
	var c jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer(challengeIssuer), jwt.WithExpirationRequired())
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(c.Subject)
}
