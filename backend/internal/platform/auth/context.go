package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Principal is the authenticated user making a request. Services read it from
// the request context to record ownership and audit entries.
type Principal struct {
	UserID   uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Position string    `json:"position"`
	Role     Role      `json:"role"`
	// IssuedAt is when the session token was issued; zero for non-HTTP callers.
	IssuedAt time.Time `json:"-"`
	// SessionID identifies the server-side session behind the token.
	SessionID uuid.UUID `json:"-"`
	// MFASetupPending is set when someone who must use two-step verification
	// signed in with only a password: they can do nothing but set it up.
	MFASetupPending bool `json:"-"`
	// Active is false when the request was made by the app in the background
	// while the person was away (so it doesn't keep an idle session alive).
	Active bool `json:"-"`
}

func (p Principal) Can(perm string) bool { return p.Role.Can(perm) }

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
