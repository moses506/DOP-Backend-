// Package users manages staff accounts and sign-in.
package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
	mailer "dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/secretbox"
)

type User struct {
	ID                 uuid.UUID  `json:"id"`
	Email              string     `json:"email"`
	FullName           string     `json:"full_name"`
	Position           string     `json:"position"`
	Department         string     `json:"department"`
	Role               auth.Role  `json:"role"`
	Active             bool       `json:"active"`
	MustChangePassword bool       `json:"must_change_password"`
	LastLoginAt        *time.Time `json:"last_login_at"`
	CreatedAt          time.Time  `json:"created_at"`
	// MFAEnabled: two-step verification is set up. MFARequired: their role must use it.
	MFAEnabled  bool `json:"mfa_enabled"`
	MFARequired bool `json:"mfa_required"`
	// Phone is the work number printed on documents such as quotations.
	Phone *string `json:"phone"`
	// ManagerID is who the person reports to.
	ManagerID *uuid.UUID `json:"manager_id"`
}

func (u User) Principal() auth.Principal {
	return auth.Principal{UserID: u.ID, Name: u.FullName, Position: u.Position, Role: u.Role}
}

type CreateInput struct {
	Email      string     `json:"email"`
	FullName   string     `json:"full_name"`
	Position   string     `json:"position"`
	Department string     `json:"department"`
	Role       auth.Role  `json:"role"`
	Password   string     `json:"password"`
	Phone      string     `json:"phone"`
	ManagerID  *uuid.UUID `json:"manager_id"`
}

// UpdateInput changes a staff member's details. Empty fields are left as they
// are, except Phone: send "" to clear it, or leave it out to keep it.
type UpdateInput struct {
	Email      string    `json:"email"`
	FullName   string    `json:"full_name"`
	Position   string    `json:"position"`
	Department string    `json:"department"`
	Role       auth.Role `json:"role"`
	Phone      *string   `json:"phone"`
	// ManagerID: send "" to clear who they report to, or leave it out to keep it.
	ManagerID *string `json:"manager_id"`
}

func (in *CreateInput) normalize() {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.FullName = strings.TrimSpace(in.FullName)
	in.Position = strings.TrimSpace(in.Position)
	in.Department = strings.TrimSpace(in.Department)
	in.Phone = strings.Join(strings.Fields(in.Phone), " ")
}

func (in CreateInput) validate() error {
	fe := httpx.FieldErrors{}
	validateProfile(fe, in.Email, in.FullName, in.Position, in.Department, in.Role)
	validatePhone(fe, in.Phone)
	if msg := PasswordProblem(in.Password, in.Email, in.FullName); msg != "" {
		fe.Add("password", msg)
	}
	return fe.Err()
}

func validateProfile(fe httpx.FieldErrors, email, name, position, dept string, role auth.Role) {
	if _, err := mail.ParseAddress(email); err != nil || !strings.Contains(email, "@") {
		fe.Add("email", "Enter a valid email address.")
	}
	if name == "" {
		fe.Add("full_name", "Full name is required.")
	}
	if position == "" {
		fe.Add("position", "Position is required.")
	}
	if dept == "" {
		fe.Add("department", "Department is required.")
	}
	if !role.Valid() {
		fe.Add("role", "Choose a valid role.")
	}
}

var (
	ErrBadCredentials = httpx.NewError(http.StatusUnauthorized, "bad_credentials", "Email or password is incorrect.")
	errSelf           = httpx.NewError(http.StatusConflict, "own_account",
		"You can't change your own role or deactivate your own account. Ask another administrator.")
	errLastAdmin = httpx.NewError(http.StatusConflict, "last_admin",
		"This is the only active administrator. Make someone else an administrator first.")
)

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
	idle time.Duration   // sessions end after this long without activity
	mail *mailer.Service // for security alerts; nil in tests
	log  *slog.Logger
	box  *secretbox.Box // encrypts authenticator secrets
}

// DefaultIdleTimeout ends a session after an hour without the person using the app.
const DefaultIdleTimeout = time.Hour

func NewService(pool *pgxpool.Pool, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{pool: pool, now: now, idle: DefaultIdleTimeout, box: secretbox.New([]byte("tests-and-tools-only"))}
}

const userCols = `id, email, full_name, position, department, role, active, must_change_password, last_login_at, created_at, (mfa_enabled_at IS NOT NULL), phone, manager_id`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.FullName, &u.Position, &u.Department, &u.Role,
		&u.Active, &u.MustChangePassword, &u.LastLoginAt, &u.CreatedAt, &u.MFAEnabled, &u.Phone, &u.ManagerID)
	u.MFARequired = MFARequiredFor(u.Role)
	return u, err
}

// Create adds a user with a password chosen by the caller (used by the admin CLI).
func (s *Service) Create(ctx context.Context, in CreateInput) (User, error) {
	return s.create(ctx, in, false)
}

// Invite adds a user who can't sign in until they open their set-up link and
// choose a password. Returns the one-time link token (never stored in plain text).
func (s *Service) Invite(ctx context.Context, in CreateInput) (User, string, error) {
	in.Password = randomSecret() // unusable until replaced through the link
	u, err := s.create(ctx, in, true)
	if err != nil {
		return User{}, "", err
	}
	tok, err := s.newSetupToken(ctx, s.pool, u.ID, InviteTTL)
	if err != nil {
		return User{}, "", err
	}
	return u, tok, nil
}

func (s *Service) create(ctx context.Context, in CreateInput, mustChange bool) (User, error) {
	in.normalize()
	if err := in.validate(); err != nil {
		return User{}, err
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return User{}, err
	}
	var u User
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if in.ManagerID != nil {
			if err := checkManager(ctx, tx, uuid.Nil, *in.ManagerID); err != nil {
				return err
			}
		}
		u, err = scanUser(tx.QueryRow(ctx, `
			INSERT INTO users (email, full_name, position, department, role, password_hash, must_change_password, phone, manager_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9) RETURNING `+userCols,
			in.Email, in.FullName, in.Position, in.Department, in.Role, hash, mustChange, in.Phone, in.ManagerID))
		if err != nil {
			if db.IsUniqueViolation(err) {
				return httpx.FieldErrors{"email": "A user with this email already exists."}.Err()
			}
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			EntityType: "user", EntityID: u.ID, Action: "created",
			Summary: "User " + u.FullName + " created with role " + string(u.Role),
		})
	})
	return u, err
}

// Authenticate checks an email and password without the guessing limits.
// Sign-in over HTTP goes through Login instead.
func (s *Service) Authenticate(ctx context.Context, email, password string) (User, error) {
	u, outcome, err := s.authenticate(ctx, email, password)
	if err == nil && outcome != "success" {
		err = ErrBadCredentials
	}
	return u, err
}

// authenticate reports the outcome: success, unknown_email, inactive or
// bad_password. err is set only for unexpected failures.
func (s *Service) authenticate(ctx context.Context, email, password string) (User, string, error) {
	var hash string
	var u User
	err := s.pool.QueryRow(ctx, `SELECT `+userCols+`, password_hash FROM users WHERE lower(email) = lower($1)`,
		strings.TrimSpace(email)).Scan(&u.ID, &u.Email, &u.FullName, &u.Position, &u.Department, &u.Role,
		&u.Active, &u.MustChangePassword, &u.LastLoginAt, &u.CreatedAt, &u.MFAEnabled, &u.Phone, &u.ManagerID, &hash)
	u.MFARequired = MFARequiredFor(u.Role)
	if db.IsNoRows(err) {
		// Spend similar time as a real check so response timing does not reveal valid emails.
		auth.CheckPassword("pbkdf2_sha256$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password)
		return User{}, "unknown_email", nil
	}
	if err != nil {
		return User{}, "", err
	}
	ok := auth.CheckPassword(hash, password)
	if !u.Active {
		return u, "inactive", nil
	}
	if !ok {
		return u, "bad_password", nil
	}
	_, _ = s.pool.Exec(ctx, `UPDATE users SET last_login_at = $2 WHERE id = $1`, u.ID, s.now())
	return u, "success", nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
	if db.IsNoRows(err) {
		return User{}, httpx.ErrNotFound
	}
	return u, err
}

func (s *Service) List(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY active DESC, full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Update changes a user's profile and role. Changing a role signs the user out
// everywhere so their next session carries the new permissions.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (User, error) {
	actor, _ := auth.FromContext(ctx)
	var u User
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1 FOR UPDATE`, id))
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		next := CreateInput{
			Email: or(in.Email, cur.Email), FullName: or(in.FullName, cur.FullName),
			Position: or(in.Position, cur.Position), Department: or(in.Department, cur.Department),
			Role: auth.Role(or(string(in.Role), string(cur.Role))),
		}
		next.Phone = derefPhone(cur.Phone)
		if in.Phone != nil {
			next.Phone = *in.Phone
		}
		next.normalize()
		fe := httpx.FieldErrors{}
		validateProfile(fe, next.Email, next.FullName, next.Position, next.Department, next.Role)
		validatePhone(fe, next.Phone)
		if err := fe.Err(); err != nil {
			return err
		}
		manager := cur.ManagerID
		if in.ManagerID != nil {
			manager = nil
			if v := strings.TrimSpace(*in.ManagerID); v != "" {
				mid, err := uuid.Parse(v)
				if err != nil {
					return httpx.FieldErrors{"manager_id": "Choose a member of staff."}.Err()
				}
				if err := checkManager(ctx, tx, id, mid); err != nil {
					return err
				}
				manager = &mid
			}
		}
		roleChanged := next.Role != cur.Role
		if roleChanged {
			if id == actor.UserID {
				return errSelf
			}
			if cur.Role == auth.RoleAdmin && cur.Active {
				if err := s.ensureAnotherAdmin(ctx, tx, id); err != nil {
					return err
				}
			}
		}

		changes := map[string]any{}
		diff := func(k, a, b string) {
			if a != b {
				changes[k] = map[string]string{"from": a, "to": b}
			}
		}
		diff("email", cur.Email, next.Email)
		diff("full_name", cur.FullName, next.FullName)
		diff("position", cur.Position, next.Position)
		diff("department", cur.Department, next.Department)
		diff("role", string(cur.Role), string(next.Role))
		diff("phone", derefPhone(cur.Phone), next.Phone)
		diff("manager_id", idString(cur.ManagerID), idString(manager))
		if len(changes) == 0 {
			u = cur
			return nil
		}

		u, err = scanUser(tx.QueryRow(ctx, `
			UPDATE users SET email = $2, full_name = $3, position = $4, department = $5, role = $6,
			       sessions_valid_after = CASE WHEN $7 THEN now() ELSE sessions_valid_after END,
			       phone = NULLIF($8, ''), manager_id = $9, updated_at = now()
			WHERE id = $1 RETURNING `+userCols,
			id, next.Email, next.FullName, next.Position, next.Department, next.Role, roleChanged, next.Phone, manager))
		if err != nil {
			if db.IsUniqueViolation(err) {
				return httpx.FieldErrors{"email": "A user with this email already exists."}.Err()
			}
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: "user", EntityID: id, Action: "updated",
			Summary: "User " + u.FullName + " updated", Changes: changes})
	})
	return u, err
}

// SetActive activates or deactivates an account. Deactivating signs the user
// out on every device immediately.
func (s *Service) SetActive(ctx context.Context, id uuid.UUID, active bool) (User, error) {
	actor, _ := auth.FromContext(ctx)
	if !active && id == actor.UserID {
		return User{}, errSelf
	}
	var u User
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1 FOR UPDATE`, id))
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if !active && cur.Role == auth.RoleAdmin && cur.Active {
			if err := s.ensureAnotherAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		u, err = scanUser(tx.QueryRow(ctx, `
			UPDATE users SET active = $2,
			       sessions_valid_after = CASE WHEN $2 THEN sessions_valid_after ELSE now() END,
			       updated_at = now()
			WHERE id = $1 RETURNING `+userCols, id, active))
		if err != nil {
			return err
		}
		action := "deactivated"
		if active {
			action = "activated"
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: "user", EntityID: id, Action: action,
			Summary: "User " + u.FullName + " " + action})
	})
	return u, err
}

// How long set-up links stay valid.
const (
	InviteTTL = 72 * time.Hour
	ResetTTL  = 24 * time.Hour
)

var errLinkInvalid = httpx.NewError(http.StatusGone, "link_invalid",
	"This link has expired or has already been used. Ask your administrator to send a new one.")

// ResetPassword locks the current password, signs the user out everywhere and
// returns a one-time link token for choosing a new one.
func (s *Service) ResetPassword(ctx context.Context, id uuid.UUID) (User, string, error) {
	hash, err := auth.HashPassword(randomSecret())
	if err != nil {
		return User{}, "", err
	}
	var u User
	var tok string
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		u, err = scanUser(tx.QueryRow(ctx, `
			UPDATE users SET password_hash = $2, must_change_password = true,
			       sessions_valid_after = now(), updated_at = now()
			WHERE id = $1 AND active RETURNING `+userCols, id, hash))
		if db.IsNoRows(err) {
			return httpx.NewError(http.StatusConflict, "inactive", "Reactivate this account before resetting its password.")
		}
		if err != nil {
			return err
		}
		if tok, err = s.newSetupToken(ctx, tx, id, ResetTTL); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: "user", EntityID: id, Action: "password_reset",
			Summary: "Password reset link issued for " + u.FullName})
	})
	if err != nil {
		return User{}, "", err
	}
	return u, tok, nil
}

// RenewInvite issues a fresh invitation link for someone who hasn't set up their account yet.
func (s *Service) RenewInvite(ctx context.Context, id uuid.UUID) (User, string, error) {
	u, err := s.Get(ctx, id)
	if err != nil {
		return User{}, "", err
	}
	if !u.Active || u.LastLoginAt != nil {
		return User{}, "", httpx.NewError(http.StatusConflict, "already_set_up",
			"This person has already set up their account. Use Reset password instead.")
	}
	tok, err := s.newSetupToken(ctx, s.pool, id, InviteTTL)
	return u, tok, err
}

// SetupLink describes who a valid set-up link belongs to.
type SetupLink struct {
	FullName string `json:"full_name"`
	Email    string `json:"email"`
	Invite   bool   `json:"invite"` // true for a first-time invitation, false for a reset
}

// LookupSetup returns the account a set-up link belongs to, if it is still valid.
func (s *Service) LookupSetup(ctx context.Context, token string) (SetupLink, error) {
	var l SetupLink
	var lastLogin *time.Time
	err := s.pool.QueryRow(ctx, `SELECT full_name, email, last_login_at FROM users
		WHERE setup_token_hash = $1 AND setup_expires_at > now() AND active`, hashToken(token)).
		Scan(&l.FullName, &l.Email, &lastLogin)
	if db.IsNoRows(err) {
		return SetupLink{}, errLinkInvalid
	}
	l.Invite = lastLogin == nil
	return l, err
}

// CompleteSetup sets the password chosen through a set-up link. The link stops
// working and any older sessions are revoked.
func (s *Service) CompleteSetup(ctx context.Context, token, password string) (User, error) {
	var email, name string
	err := s.pool.QueryRow(ctx, `SELECT email, full_name FROM users
		WHERE setup_token_hash = $1 AND setup_expires_at > now() AND active`, hashToken(token)).Scan(&email, &name)
	if db.IsNoRows(err) {
		return User{}, errLinkInvalid
	}
	if err != nil {
		return User{}, err
	}
	if msg := PasswordProblem(password, email, name); msg != "" {
		return User{}, httpx.FieldErrors{"password": msg}.Err()
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return User{}, err
	}
	var u User
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		u, err = scanUser(tx.QueryRow(ctx, `
			UPDATE users SET password_hash = $2, must_change_password = false, password_changed_at = now(),
			       setup_token_hash = NULL, setup_expires_at = NULL,
			       sessions_valid_after = now(), last_login_at = now(), updated_at = now()
			WHERE setup_token_hash = $1 AND setup_expires_at > now() AND active
			RETURNING `+userCols, hashToken(token), hash))
		if db.IsNoRows(err) {
			return errLinkInvalid
		}
		if err != nil {
			return err
		}
		ctx := auth.WithPrincipal(ctx, u.Principal())
		return audit.Record(ctx, tx, audit.Entry{EntityType: "user", EntityID: u.ID, Action: "password_set",
			Summary: u.FullName + " set their password from an emailed link"})
	})
	return u, err
}

func (s *Service) newSetupToken(ctx context.Context, q db.Querier, id uuid.UUID, ttl time.Duration) (string, error) {
	tok := randomSecret()
	_, err := q.Exec(ctx, `UPDATE users SET setup_token_hash = $2, setup_expires_at = $3 WHERE id = $1`,
		id, hashToken(tok), s.now().Add(ttl))
	return tok, err
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

// randomSecret returns 256 bits of randomness, URL-safe.
func randomSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ChangeOwnPassword lets a signed-in user replace their password. Other
// sessions are signed out; the caller issues a fresh token.
func (s *Service) ChangeOwnPassword(ctx context.Context, id uuid.UUID, current, next string) (User, error) {
	var hash, email, name string
	if err := s.pool.QueryRow(ctx, `SELECT password_hash, email, full_name FROM users WHERE id = $1 AND active`, id).Scan(&hash, &email, &name); err != nil {
		if db.IsNoRows(err) {
			return User{}, httpx.ErrUnauthorized
		}
		return User{}, err
	}
	if !auth.CheckPassword(hash, current) {
		return User{}, httpx.FieldErrors{"current_password": "That isn't your current password."}.Err()
	}
	if next == current {
		return User{}, httpx.FieldErrors{"new_password": "Choose a password different from your current one."}.Err()
	}
	if msg := PasswordProblem(next, email, name); msg != "" {
		return User{}, httpx.FieldErrors{"new_password": msg}.Err()
	}
	newHash, err := auth.HashPassword(next)
	if err != nil {
		return User{}, err
	}
	var u User
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		u, err = scanUser(tx.QueryRow(ctx, `
			UPDATE users SET password_hash = $2, must_change_password = false, password_changed_at = now(),
			       sessions_valid_after = now(), updated_at = now()
			WHERE id = $1 RETURNING `+userCols, id, newHash))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: "user", EntityID: id, Action: "password_changed",
			Summary: u.FullName + " changed their password"})
	})
	return u, err
}

func (s *Service) ensureAnotherAdmin(ctx context.Context, tx pgx.Tx, except uuid.UUID) error {
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE role = 'admin' AND active AND id <> $1`, except).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errLastAdmin
	}
	return nil
}

func or(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func derefPhone(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// validatePhone accepts numbers like "+260 977 415067" or "(+260) 97 4954929".
func validatePhone(fe httpx.FieldErrors, phone string) {
	if phone == "" {
		return
	}
	digits := 0
	for _, r := range phone {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case strings.ContainsRune("+()- /", r):
		default:
			fe.Add("phone", "Use digits, spaces and + ( ) - only, e.g. +260 977 415067.")
			return
		}
	}
	if digits < 7 || len(phone) > 30 {
		fe.Add("phone", "Enter a full phone number, e.g. +260 977 415067.")
	}
}

func idString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// checkManager makes sure the new manager is an active member of staff and
// that the reporting line doesn't loop back to the person (self is uuid.Nil for new staff).
func checkManager(ctx context.Context, q db.Querier, self, manager uuid.UUID) error {
	if manager == self {
		return httpx.FieldErrors{"manager_id": "Someone can't report to themselves."}.Err()
	}
	var active, loops bool
	err := q.QueryRow(ctx, `
		WITH RECURSIVE chain AS (
		    SELECT id, manager_id, 1 AS depth FROM users WHERE id = $1
		    UNION ALL
		    SELECT u.id, u.manager_id, c.depth + 1 FROM users u JOIN chain c ON u.id = c.manager_id WHERE c.depth < 50
		)
		SELECT coalesce((SELECT active FROM users WHERE id = $1), false), EXISTS (SELECT 1 FROM chain WHERE id = $2)`, manager, self).Scan(&active, &loops)
	if err == nil && !active {
		return httpx.FieldErrors{"manager_id": "Choose an active member of staff."}.Err()
	}
	if err != nil {
		return err
	}
	if loops {
		return httpx.FieldErrors{"manager_id": "That would make a loop — this person is above them in the reporting line."}.Err()
	}
	return nil
}
