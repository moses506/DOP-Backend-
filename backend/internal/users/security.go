package users

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/mail"
)

// Sign-in protection.
//
// Password guessing is stopped two ways, both counted from login_attempts:
//   - per account: after MaxFailedLogins wrong passwords within LockWindow,
//     sign-in for that email is paused until the window passes, an
//     administrator resets the password, or the person uses a set-up link.
//     Emails that have no account are paused the same way, so the response
//     never reveals whether an account exists;
//   - per network address: NetworkMaxFailures failures within LockWindow from
//     one IP pause sign-in from it, to stop one machine trying many accounts.
const (
	MaxFailedLogins    = 5
	NetworkMaxFailures = 30
	LockWindow         = 15 * time.Minute
)

var (
	ErrAccountLocked = httpx.NewError(http.StatusTooManyRequests, "too_many_attempts",
		"Too many failed sign-in attempts. For your security, sign-in to this account is paused for 15 minutes. "+
			"If you've forgotten your password, ask an administrator to reset it.")
	ErrNetworkLimited = httpx.NewError(http.StatusTooManyRequests, "too_many_attempts",
		"Too many failed sign-in attempts from this network. Please wait 15 minutes and try again.")
)

// Attempt describes where a sign-in came from.
type Attempt struct {
	Email     string
	Password  string
	IP        string
	UserAgent string
}

// Login checks the password with the guessing limits applied, and records the attempt.
func (s *Service) Login(ctx context.Context, a Attempt) (User, error) {
	key := emailKey(a.Email)
	var ipFails int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM login_attempts
		WHERE ip = $1 AND at > now() - $2::interval AND outcome IN ('bad_password','unknown_email','inactive','bad_code')`,
		a.IP, LockWindow.String()).Scan(&ipFails); err != nil {
		return User{}, err
	}
	if ipFails >= NetworkMaxFailures {
		s.record(ctx, key, nil, a, "rate_limited")
		return User{}, ErrNetworkLimited
	}
	fails, err := s.recentFailures(ctx, key)
	if err != nil {
		return User{}, err
	}
	if fails >= MaxFailedLogins {
		s.record(ctx, key, nil, a, "locked")
		return User{}, ErrAccountLocked
	}

	u, outcome, err := s.authenticate(ctx, a.Email, a.Password)
	if err != nil && outcome == "" {
		return User{}, err
	}
	var uid *uuid.UUID
	if u.ID != uuid.Nil {
		uid = &u.ID
	}
	recorded := outcome
	if outcome == "success" && u.MFAEnabled {
		// Not a success yet: the code is still to come, so earlier failures keep counting.
		recorded = "code_required"
	}
	s.record(ctx, key, uid, a, recorded)
	if outcome == "success" {
		return u, nil
	}
	if fails+1 >= MaxFailedLogins {
		if outcome == "bad_password" && s.mail != nil {
			// Tell the owner, in case it wasn't them. Don't hold up the response.
			go s.sendLockedEmail(context.WithoutCancel(ctx), u, a)
		}
		return User{}, ErrAccountLocked
	}
	return User{}, ErrBadCredentials
}

// Unlock clears the failed-attempt count for an account, e.g. after an
// administrator resets the password or the person sets one from a link.
func (s *Service) Unlock(ctx context.Context, email string) {
	s.record(ctx, emailKey(email), nil, Attempt{IP: "system"}, "unlocked")
}

func (s *Service) recentFailures(ctx context.Context, key string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM login_attempts
		WHERE email_key = $1 AND outcome IN ('bad_password','unknown_email','inactive','bad_code')
		  AND at > now() - $2::interval
		  AND at > coalesce((SELECT max(at) FROM login_attempts
		                     WHERE email_key = $1 AND outcome IN ('success','unlocked')), 'epoch')`,
		key, LockWindow.String()).Scan(&n)
	return n, err
}

func (s *Service) record(ctx context.Context, key string, uid *uuid.UUID, a Attempt, outcome string) {
	ua := a.UserAgent
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO login_attempts (email_key, user_id, ip, user_agent, outcome)
		VALUES ($1,$2,$3,NULLIF($4,''),$5)`, key, uid, a.IP, ua, outcome); err != nil && s.log != nil {
		s.log.Error("login attempt not recorded", "err", err)
	}
}

func emailKey(email string) string {
	k := strings.ToLower(strings.TrimSpace(email))
	if len(k) > 254 {
		k = k[:254]
	}
	return k
}

func (s *Service) sendLockedEmail(ctx context.Context, u User, a Attempt) {
	html, text := mail.Render(mail.Content{
		Preheader: "Several wrong passwords were entered for your account.",
		Heading:   "Sign-in paused on your account",
		Paragraphs: []string{
			"Hi " + firstName(u.FullName) + ", someone entered the wrong password for your DOS BPA account " +
				strconv.Itoa(MaxFailedLogins) + " times, so we've paused sign-in to it for 15 minutes.",
			"If that was you, wait a few minutes and try again, or ask an administrator to reset your password. " +
				"If it wasn't you, tell your administrator straight away — someone may be trying to get into your account.",
		},
		Details: [][2]string{
			{"Time", time.Now().In(s.loc()).Format("2 Jan 2006, 15:04")},
			{"Network address", a.IP},
			{"Device", shorten(a.UserAgent, 90)},
		},
		Footnote: "Your password has not been changed and nobody has signed in.",
	})
	if err := s.mail.Send(ctx, mail.Message{To: u.Email, ToName: u.FullName, Kind: "login_locked",
		Subject: "Sign-in to your DOS BPA account was paused", HTML: html, Text: text}); err != nil && s.log != nil {
		s.log.Warn("lockout email not sent", "err", err)
	}
}

// ---- sessions ----

// NewSession opens a server-side session for a successful sign-in.
// needsMFASetup limits it to setting up two-step verification.
func (s *Service) NewSession(ctx context.Context, userID uuid.UUID, ip, userAgent string, ttl time.Duration, needsMFASetup ...bool) (uuid.UUID, time.Time, error) {
	var id uuid.UUID
	var exp time.Time
	if len(userAgent) > 300 {
		userAgent = userAgent[:300]
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, expires_at, ip, user_agent, needs_mfa_setup)
		VALUES ($1, now() + $2::interval, NULLIF($3,''), NULLIF($4,''), $5)
		RETURNING id, expires_at`, userID, ttl.String(), ip, userAgent, len(needsMFASetup) > 0 && needsMFASetup[0]).Scan(&id, &exp)
	return id, exp, err
}

// CheckSession runs on every signed-in request. It rejects the request if the
// account was deactivated, its sessions were revoked (password, role change),
// or this session was signed out, expired, or left idle too long.
func (s *Service) CheckSession(ctx context.Context, p *auth.Principal) error {
	var active bool
	var validAfter time.Time
	var exp, lastSeen *time.Time
	var revoked *time.Time
	var needsMFA *bool
	err := s.pool.QueryRow(ctx, `
		SELECT u.active, coalesce(date_trunc('second', u.sessions_valid_after), 'epoch'),
		       se.expires_at, se.last_seen_at, se.revoked_at, se.needs_mfa_setup
		FROM users u LEFT JOIN sessions se ON se.id = $2 AND se.user_id = u.id
		WHERE u.id = $1`, p.UserID, p.SessionID).Scan(&active, &validAfter, &exp, &lastSeen, &revoked, &needsMFA)
	if db.IsNoRows(err) {
		return auth.ErrSessionRevoked
	}
	if err != nil {
		return err
	}
	now := s.now()
	switch {
	case !active, exp == nil, revoked != nil, now.After(*exp):
		return auth.ErrSessionRevoked
	case !p.IssuedAt.IsZero() && p.IssuedAt.Before(validAfter):
		return auth.ErrSessionRevoked
	case now.Sub(*lastSeen) > s.idle:
		_ = s.RevokeSession(ctx, p.SessionID, "idle")
		return auth.ErrSessionRevoked
	}
	p.MFASetupPending = needsMFA != nil && *needsMFA
	// Only requests made while the person is at the screen keep the session alive.
	if p.Active && now.Sub(*lastSeen) > 30*time.Second {
		_, _ = s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = now() WHERE id = $1`, p.SessionID)
	}
	return nil
}

func (s *Service) RevokeSession(ctx context.Context, id uuid.UUID, why string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = now(), revoked_why = $2 WHERE id = $1 AND revoked_at IS NULL`, id, why)
	return err
}

// RevokeAllSessions signs the person out on every device.
func (s *Service) RevokeAllSessions(ctx context.Context, userID uuid.UUID, why string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = now(), revoked_why = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, why)
	return err
}

// IdleTimeout is how long a session may go without the person using the app.
func (s *Service) IdleTimeout() time.Duration { return s.idle }

// Start prunes old sign-in records in the background: attempts after 180
// days, sessions 30 days after they ended.
func (m *Module) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			_, _ = m.svc.pool.Exec(ctx, `DELETE FROM login_attempts WHERE at < now() - interval '180 days'`)
			_, _ = m.svc.pool.Exec(ctx, `DELETE FROM sessions WHERE coalesce(revoked_at, expires_at) < now() - interval '30 days'`)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (s *Service) loc() *time.Location {
	if l, err := time.LoadLocation("Africa/Lusaka"); err == nil {
		return l
	}
	return time.UTC
}

func shorten(s string, n int) string {
	if s == "" {
		return "Unknown"
	}
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// WithClock returns a copy of the service that reads the time from now (for tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	c := *s
	c.now = now
	return &c
}

// RecordBadCode notes a wrong two-step code against the account (for tests and tools).
func (s *Service) RecordBadCode(ctx context.Context, email, ip string) {
	s.record(ctx, emailKey(email), nil, Attempt{Email: email, IP: ip}, "bad_code")
}

// Pool exposes the database pool to tests in this package's test helpers.
func (s *Service) Pool() *pgxpool.Pool { return s.pool }
