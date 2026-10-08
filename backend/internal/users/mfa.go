package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
	mailer "dos-bpas/internal/platform/mail"
)

// Two-step verification: after the password, a 6-digit code from an
// authenticator app (or a one-time recovery code). The app's secret is stored
// encrypted, codes can't be replayed, and wrong codes count towards the same
// lockout as wrong passwords.

const (
	MFAIssuer         = "DOS BPA"
	RecoveryCodeCount = 10
)

// mfaRoles are the roles that must use two-step verification (MFA_REQUIRED_ROLES).
var mfaRoles = map[auth.Role]bool{auth.RoleAdmin: true, auth.RoleCEO: true, auth.RoleSalesManager: true}

// SetMFARequiredRoles replaces the roles that must use two-step verification.
func SetMFARequiredRoles(roles []string) {
	m := map[auth.Role]bool{}
	for _, r := range roles {
		if role := auth.Role(strings.TrimSpace(r)); role.Valid() {
			m[role] = true
		}
	}
	mfaRoles = m
}

func MFARequiredFor(r auth.Role) bool { return mfaRoles[r] }

var (
	ErrBadCode = httpx.NewError(http.StatusUnauthorized, "bad_code",
		"That code didn't work. Check the 6-digit code in your authenticator app and try again.")
	errChallenge = httpx.NewError(http.StatusUnauthorized, "challenge_expired",
		"Sign-in took too long. Please enter your email and password again.")
	errMFAOn       = httpx.NewError(http.StatusConflict, "mfa_already_on", "Two-step verification is already on for your account.")
	errMFAOff      = httpx.NewError(http.StatusConflict, "mfa_off", "Two-step verification isn't on for this account.")
	errMFARequired = httpx.NewError(http.StatusConflict, "mfa_required",
		"Two-step verification is required for your role, so it can't be turned off. If you've lost your phone, ask an administrator to reset it.")
	errSetupExpired = httpx.NewError(http.StatusConflict, "setup_expired", "The set-up took too long. Start again to get a new QR code.")
)

// BeginMFASetup creates a new secret for the person to add to their app. It
// only takes effect once they confirm a code from it (EnableMFA).
func (s *Service) BeginMFASetup(ctx context.Context, userID uuid.UUID) (key, uri string, err error) {
	u, err := s.Get(ctx, userID)
	if err != nil {
		return "", "", err
	}
	if u.MFAEnabled {
		return "", "", errMFAOn
	}
	secret := auth.NewTOTPSecret()
	if _, err := s.pool.Exec(ctx, `UPDATE users SET mfa_pending_secret = $2, mfa_pending_until = now() + interval '15 minutes' WHERE id = $1`,
		userID, s.box.Seal(secret)); err != nil {
		return "", "", err
	}
	return auth.TOTPKey(secret), auth.TOTPURI(secret, MFAIssuer, u.Email), nil
}

// EnableMFA turns two-step verification on once the person proves their app
// shows the right code, and returns their one-time recovery codes.
func (s *Service) EnableMFA(ctx context.Context, userID uuid.UUID, code string) ([]string, error) {
	var codes []string
	var u User
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var pending []byte
		var until *time.Time
		var enabled bool
		err := tx.QueryRow(ctx, `SELECT mfa_pending_secret, mfa_pending_until, mfa_enabled_at IS NOT NULL FROM users WHERE id = $1 FOR UPDATE`,
			userID).Scan(&pending, &until, &enabled)
		if err != nil {
			return err
		}
		if enabled {
			return errMFAOn
		}
		if pending == nil || until == nil || s.now().After(*until) {
			return errSetupExpired
		}
		secret, err := s.box.Open(pending)
		if err != nil {
			return err
		}
		step, ok := auth.VerifyTOTP(secret, code, s.now(), 0)
		if !ok {
			return httpx.FieldErrors{"code": "That code didn't match. Make sure you scanned the latest QR code and that your phone's clock is right."}.Err()
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET mfa_secret = mfa_pending_secret, mfa_enabled_at = now(), mfa_last_step = $2,
			mfa_pending_secret = NULL, mfa_pending_until = NULL, updated_at = now() WHERE id = $1`, userID, step); err != nil {
			return err
		}
		if codes, err = newRecoveryCodes(ctx, tx, userID); err != nil {
			return err
		}
		u, err = scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, userID))
		if err != nil {
			return err
		}
		return audit.Record(withActor(ctx, u), tx, audit.Entry{EntityType: "user", EntityID: userID, Action: "mfa_enabled",
			Summary: u.FullName + " turned on two-step verification"})
	})
	if err != nil {
		return nil, err
	}
	s.securityEmail(u, "Two-step verification is now on",
		"Two-step verification was turned on for your DOS BPA account. From now on you'll enter a code from your authenticator app after your password.")
	return codes, nil
}

// VerifySecondFactor checks a code from the app, or a recovery code. It
// reports whether a recovery code was used.
func (s *Service) VerifySecondFactor(ctx context.Context, userID uuid.UUID, code string) (usedRecovery bool, err error) {
	code = strings.TrimSpace(code)
	if isRecoveryCode(code) {
		tag, err := s.pool.Exec(ctx, `UPDATE mfa_recovery_codes SET used_at = now()
			WHERE id = (SELECT id FROM mfa_recovery_codes WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL LIMIT 1)`,
			userID, hashRecovery(code))
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() == 0 {
			return false, ErrBadCode
		}
		u, _ := s.Get(ctx, userID)
		_ = audit.Record(withActor(ctx, u), s.pool, audit.Entry{EntityType: "user", EntityID: userID, Action: "mfa_recovery_used",
			Summary: u.FullName + " signed in with a recovery code"})
		return true, nil
	}
	var sealed []byte
	var last *int64
	if err := s.pool.QueryRow(ctx, `SELECT mfa_secret, mfa_last_step FROM users WHERE id = $1 AND mfa_enabled_at IS NOT NULL`,
		userID).Scan(&sealed, &last); err != nil {
		if db.IsNoRows(err) {
			return false, errMFAOff
		}
		return false, err
	}
	secret, err := s.box.Open(sealed)
	if err != nil {
		return false, err
	}
	var lastStep int64
	if last != nil {
		lastStep = *last
	}
	step, ok := auth.VerifyTOTP(secret, code, s.now(), lastStep)
	if !ok {
		return false, ErrBadCode
	}
	// Record the step only if no other request used it first, so one code
	// can't sign in twice.
	tag, err := s.pool.Exec(ctx, `UPDATE users SET mfa_last_step = $2 WHERE id = $1 AND (mfa_last_step IS NULL OR mfa_last_step < $2)`, userID, step)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, ErrBadCode
	}
	return false, nil
}

// DisableMFA turns two-step verification off, if the person's role allows it.
func (s *Service) DisableMFA(ctx context.Context, userID uuid.UUID, password, code string) error {
	var hash string
	var u User
	if err := s.pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&hash); err != nil {
		return err
	}
	u, err := s.Get(ctx, userID)
	if err != nil {
		return err
	}
	if !u.MFAEnabled {
		return errMFAOff
	}
	if u.MFARequired {
		return errMFARequired
	}
	if !auth.CheckPassword(hash, password) {
		return httpx.FieldErrors{"password": "That isn't your current password."}.Err()
	}
	if _, err := s.VerifySecondFactor(ctx, userID, code); err != nil {
		return httpx.FieldErrors{"code": "That code didn't work."}.Err()
	}
	if err := s.clearMFA(ctx, u, "mfa_disabled", u.FullName+" turned off two-step verification"); err != nil {
		return err
	}
	s.securityEmail(u, "Two-step verification was turned off",
		"Two-step verification was turned off for your DOS BPA account, so only your password is needed to sign in.")
	return nil
}

// ResetMFA is for a lost phone: an administrator clears someone's two-step
// verification, signs them out everywhere, and they set it up again at their
// next sign-in. Nobody can reset their own.
func (s *Service) ResetMFA(ctx context.Context, targetID uuid.UUID) (User, error) {
	actor, _ := auth.FromContext(ctx)
	if actor.UserID == targetID {
		return User{}, httpx.NewError(http.StatusConflict, "own_account",
			"You can't reset your own two-step verification. Ask another administrator, or use one of your recovery codes.")
	}
	u, err := s.Get(ctx, targetID)
	if err != nil {
		return User{}, err
	}
	if !u.MFAEnabled {
		return User{}, errMFAOff
	}
	if err := s.clearMFA(ctx, u, "mfa_reset", "Two-step verification reset for "+u.FullName+" by "+actor.Name); err != nil {
		return User{}, err
	}
	if err := s.RevokeAllSessions(ctx, targetID, "mfa_reset"); err != nil {
		return User{}, err
	}
	s.securityEmail(u, "Your two-step verification was reset",
		actor.Name+" reset two-step verification on your DOS BPA account. You'll be asked to set it up again the next time you sign in. If you didn't ask for this, tell your administrator straight away.")
	return s.Get(ctx, targetID)
}

// RegenerateRecoveryCodes replaces all recovery codes; the old ones stop working.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, userID uuid.UUID, code string) ([]string, error) {
	if isRecoveryCode(code) {
		return nil, httpx.FieldErrors{"code": "Use a code from your authenticator app, not a recovery code."}.Err()
	}
	if _, err := s.VerifySecondFactor(ctx, userID, code); err != nil {
		return nil, httpx.FieldErrors{"code": "That code didn't work."}.Err()
	}
	var codes []string
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if codes, err = newRecoveryCodes(ctx, tx, userID); err != nil {
			return err
		}
		u, _ := s.Get(ctx, userID)
		return audit.Record(withActor(ctx, u), tx, audit.Entry{EntityType: "user", EntityID: userID, Action: "mfa_recovery_regenerated",
			Summary: u.FullName + " created new recovery codes"})
	})
	return codes, err
}

func (s *Service) RecoveryCodesLeft(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM mfa_recovery_codes WHERE user_id = $1 AND used_at IS NULL`, userID).Scan(&n)
	return n, err
}

func (s *Service) clearMFA(ctx context.Context, u User, action, summary string) error {
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET mfa_secret = NULL, mfa_enabled_at = NULL, mfa_last_step = NULL,
			mfa_pending_secret = NULL, mfa_pending_until = NULL, updated_at = now() WHERE id = $1`, u.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, u.ID); err != nil {
			return err
		}
		if _, ok := auth.FromContext(ctx); !ok {
			ctx = withActor(ctx, u)
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: "user", EntityID: u.ID, Action: action, Summary: summary})
	})
}

// ---- recovery codes ----

const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no 0/o, 1/l/i

func newRecoveryCodes(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]string, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return nil, err
	}
	codes := make([]string, RecoveryCodeCount)
	for i := range codes {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		var sb strings.Builder
		for j, x := range b {
			if j == 4 {
				sb.WriteByte('-')
			}
			sb.WriteByte(recoveryAlphabet[int(x)%len(recoveryAlphabet)])
		}
		codes[i] = sb.String()
		if _, err := tx.Exec(ctx, `INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, $2)`, userID, hashRecovery(codes[i])); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

func normalizeRecovery(code string) string {
	return strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
}

func isRecoveryCode(code string) bool {
	c := normalizeRecovery(code)
	if len(c) != 8 {
		return false
	}
	for _, r := range c {
		if r < '0' || r > '9' {
			return true // app codes are digits only
		}
	}
	return false
}

func hashRecovery(code string) []byte {
	h := sha256.Sum256([]byte("dos-recovery:" + normalizeRecovery(code)))
	return h[:]
}

// ---- where you're signed in ----

type SessionInfo struct {
	ID         uuid.UUID `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	Current    bool      `json:"current"`
}

// ListSessions shows the person where they're signed in.
func (s *Service) ListSessions(ctx context.Context, userID, current uuid.UUID) ([]SessionInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, created_at, last_seen_at, coalesce(ip,''), coalesce(user_agent,'') FROM sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now() AND last_seen_at > now() - $2::interval
		ORDER BY last_seen_at DESC`, userID, s.idle.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionInfo{}
	for rows.Next() {
		var si SessionInfo
		if err := rows.Scan(&si.ID, &si.CreatedAt, &si.LastSeenAt, &si.IP, &si.UserAgent); err != nil {
			return nil, err
		}
		si.Current = si.ID == current
		out = append(out, si)
	}
	return out, rows.Err()
}

// RevokeOwnSession signs out one of the person's own sessions.
func (s *Service) RevokeOwnSession(ctx context.Context, userID, sid uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = now(), revoked_why = 'signed_out_remotely'
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, sid, userID)
	return err
}

// RevokeOtherSessions signs out every session but the current one.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID, keep uuid.UUID, why string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = now(), revoked_why = $3
		WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL`, userID, keep, why)
	return err
}

// MarkSessionVerified lifts the set-up-only limit once two-step verification is on.
func (s *Service) MarkSessionVerified(ctx context.Context, sid uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET needs_mfa_setup = false WHERE id = $1`, sid)
	return err
}

// ---- helpers ----

func withActor(ctx context.Context, u User) context.Context {
	if _, ok := auth.FromContext(ctx); ok {
		return ctx
	}
	return auth.WithPrincipal(ctx, u.Principal())
}

// securityEmail tells the person about a change to how their account is protected.
func (s *Service) securityEmail(u User, subject, text string) {
	if s.mail == nil {
		return
	}
	html, plain := mailer.Render(mailer.Content{
		Preheader:  subject,
		Heading:    subject,
		Paragraphs: []string{"Hi " + firstName(u.FullName) + ",", text},
		Details:    [][2]string{{"Time", time.Now().In(s.loc()).Format("2 Jan 2006, 15:04")}},
		Footnote:   "If you didn't make this change, tell your administrator straight away.",
	})
	go func() {
		if err := s.mail.Send(context.Background(), mailer.Message{To: u.Email, ToName: u.FullName, Kind: "security_alert",
			Subject: subject, HTML: html, Text: plain}); err != nil && s.log != nil {
			s.log.Warn("security email not sent", "err", err)
		}
	}()
}
