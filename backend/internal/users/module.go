package users

import (
	"errors"
	"io/fs"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"golang.org/x/time/rate"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/module"
	"dos-bpas/internal/platform/secretbox"
)

const (
	PermRead   = "users:read"
	PermManage = "users:manage"
)

type Module struct {
	svc       *Service
	tokens    *auth.Tokens
	now       func() time.Time
	knownPerm func() []string
	mail      *mail.Service
	appURL    string
}

// New builds the module. knownPerms returns every permission declared by all
// registered modules, used to tell the frontend what the user may do.
func New(d module.Deps, knownPerms func() []string) *Module {
	m := &Module{svc: NewService(d.DB, d.Now), tokens: d.Tokens, now: d.Now, knownPerm: knownPerms,
		mail: d.Mail, appURL: d.AppURL}
	m.svc.mail, m.svc.log = d.Mail, d.Log
	if len(d.DataKey) > 0 {
		m.svc.box = secretbox.New(d.DataKey)
	}
	if d.MFARoles != nil {
		SetMFARequiredRoles(d.MFARoles)
	}
	if d.SessionIdle > 0 {
		m.svc.idle = d.SessionIdle
	}
	// Every authenticated request confirms the account is still active and the
	// token wasn't revoked by a deactivation, password reset or role change.
	d.Tokens.SetCheck(m.svc.CheckSession)
	return m
}

func (m *Module) Service() *Service     { return m.svc }
func (m *Module) Name() string          { return "users" }
func (m *Module) Migrations() fs.FS     { return nil } // users table is part of platform migrations
func (m *Module) Permissions() []string { return []string{PermRead, PermManage} }

func (m *Module) Routes(r module.Router) {
	// Sign-in and set-up links are open to anyone, so each network address is
	// also limited in how fast it can call them (on top of the account lockout).
	limit := authRateLimit()
	r.Public.POST("/auth/login", m.login, limit)
	r.Public.GET("/auth/setup", m.lookupSetup, limit)
	r.Public.POST("/auth/setup", m.completeSetup, limit)
	r.Authed.GET("/auth/me", m.me)
	r.Authed.POST("/auth/change-password", m.changePassword)
	r.Authed.POST("/auth/logout", m.logout)
	r.Authed.POST("/auth/logout-all", m.logoutAll)

	// Two-step verification.
	r.Public.POST("/auth/mfa/verify", m.verifyCode, limit)
	r.Authed.GET("/auth/security", m.security)
	r.Authed.POST("/auth/mfa/setup", m.mfaSetup)
	r.Authed.POST("/auth/mfa/enable", m.mfaEnable)
	r.Authed.POST("/auth/mfa/disable", m.mfaDisable)
	r.Authed.POST("/auth/mfa/recovery-codes", m.mfaRecoveryCodes)
	r.Authed.DELETE("/auth/sessions/:id", m.endSession)

	g := r.Authed.Group("/users")
	g.GET("", m.list, auth.Require(PermRead))
	// Everyone signed in can see who their active colleagues are (name and
	// position only), to hand them work such as a follow-up.
	g.GET("/directory", m.directory)
	g.POST("", m.create, auth.Require(PermManage))
	g.PATCH("/:id", m.update, auth.Require(PermManage))
	g.POST("/:id/activate", m.setActive(true), auth.Require(PermManage))
	g.POST("/:id/deactivate", m.setActive(false), auth.Require(PermManage))
	g.POST("/:id/reset-password", m.resetPassword, auth.Require(PermManage))
	g.POST("/:id/resend-invite", m.resendInvite, auth.Require(PermManage))
	g.POST("/:id/reset-mfa", m.resetMFA, auth.Require(PermManage))
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// session is what the web app learns about a sign-in. The token itself is
// only ever sent as an HttpOnly cookie, never in the body.
type session struct {
	ExpiresAt   time.Time `json:"expires_at"`
	IdleSeconds int       `json:"idle_timeout_seconds"`
	User        User      `json:"user"`
	Permissions []string  `json:"permissions"`
	// MFASetupRequired: the person must set up two-step verification before anything else.
	MFASetupRequired bool `json:"mfa_setup_required,omitempty"`
	// RecoveryLeft is set after signing in with a recovery code.
	RecoveryLeft *int `json:"recovery_codes_left,omitempty"`
}

func (m *Module) login(c echo.Context) error {
	var req loginReq
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if len(req.Password) > MaxPasswordLength {
		return ErrBadCredentials
	}
	u, err := m.svc.Login(c.Request().Context(), Attempt{
		Email: req.Email, Password: req.Password, IP: c.RealIP(), UserAgent: c.Request().UserAgent(),
	})
	if err != nil {
		return err
	}
	return m.afterPassword(c, u)
}

// afterPassword continues a sign-in once the password is right: people with
// two-step verification get a challenge for their code; everyone else is signed
// in (limited to setting it up, if their role requires it).
func (m *Module) afterPassword(c echo.Context, u User) error {
	if u.MFAEnabled {
		ch, err := m.tokens.IssueChallenge(u.ID, m.now())
		if err != nil {
			return err
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, map[string]any{"mfa_required": true, "challenge": ch})
	}
	return m.issue(c, u, u.MFARequired, nil)
}

type verifyReq struct {
	Challenge string `json:"challenge"`
	Code      string `json:"code"`
}

// verifyCode is the second step of signing in.
func (m *Module) verifyCode(c echo.Context) error {
	var req verifyReq
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ctx := c.Request().Context()
	uid, err := m.tokens.ParseChallenge(req.Challenge)
	if err != nil {
		return errChallenge
	}
	u, err := m.svc.Get(ctx, uid)
	if err != nil || !u.Active {
		return errChallenge
	}
	key, a := emailKey(u.Email), attemptFrom(c, u.Email)
	fails, err := m.svc.recentFailures(ctx, key)
	if err != nil {
		return err
	}
	if fails >= MaxFailedLogins {
		m.svc.record(ctx, key, &u.ID, a, "locked")
		return ErrAccountLocked
	}
	usedRecovery, err := m.svc.VerifySecondFactor(ctx, uid, req.Code)
	if errors.Is(err, ErrBadCode) {
		m.svc.record(ctx, key, &u.ID, a, "bad_code")
		if fails+1 >= MaxFailedLogins {
			return ErrAccountLocked
		}
		return ErrBadCode
	}
	if err != nil {
		return err
	}
	m.svc.record(ctx, key, &u.ID, a, "success")
	var left *int
	if usedRecovery {
		n, _ := m.svc.RecoveryCodesLeft(ctx, uid)
		left = &n
	}
	return m.issue(c, u, false, left)
}

func attemptFrom(c echo.Context, email string) Attempt {
	return Attempt{Email: email, IP: c.RealIP(), UserAgent: c.Request().UserAgent()}
}

// issue opens a session for u and sets the session cookie. needsMFASetup
// limits the session to setting up two-step verification.
func (m *Module) issue(c echo.Context, u User, needsMFASetup bool, recoveryLeft *int) error {
	ctx := c.Request().Context()
	sid, exp, err := m.svc.NewSession(ctx, u.ID, c.RealIP(), c.Request().UserAgent(), m.tokens.TTL(), needsMFASetup)
	if err != nil {
		return err
	}
	p := u.Principal()
	p.SessionID = sid
	tok, _, err := m.tokens.Issue(p, m.now())
	if err != nil {
		return err
	}
	m.tokens.SetCookie(c, tok, exp)
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, session{ExpiresAt: exp, IdleSeconds: int(m.svc.IdleTimeout().Seconds()),
		User: u, Permissions: u.Role.Expand(m.knownPerm()), MFASetupRequired: needsMFASetup, RecoveryLeft: recoveryLeft})
}

// logout ends this session on the server and removes the cookie.
func (m *Module) logout(c echo.Context) error {
	p, _ := auth.FromContext(c.Request().Context())
	if err := m.svc.RevokeSession(c.Request().Context(), p.SessionID, "signed_out"); err != nil {
		return err
	}
	m.tokens.ClearCookie(c)
	return c.NoContent(http.StatusNoContent)
}

// logoutAll ends every session of the caller, on all devices.
func (m *Module) logoutAll(c echo.Context) error {
	p, _ := auth.FromContext(c.Request().Context())
	if err := m.svc.RevokeAllSessions(c.Request().Context(), p.UserID, "signed_out_everywhere"); err != nil {
		return err
	}
	m.tokens.ClearCookie(c)
	return c.NoContent(http.StatusNoContent)
}

type changePasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// changePassword replaces the caller's password and returns a fresh session,
// because every earlier token for this account is revoked.
func (m *Module) changePassword(c echo.Context) error {
	var req changePasswordReq
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	p, _ := auth.FromContext(c.Request().Context())
	u, err := m.svc.ChangeOwnPassword(c.Request().Context(), p.UserID, req.CurrentPassword, req.NewPassword)
	if err != nil {
		return err
	}
	// Sign out every other device; this one gets a fresh session.
	if err := m.svc.RevokeAllSessions(c.Request().Context(), u.ID, "password_changed"); err != nil {
		return err
	}
	return m.issue(c, u, p.MFASetupPending, nil)
}

func (m *Module) me(c echo.Context) error {
	p, _ := auth.FromContext(c.Request().Context())
	u, err := m.svc.Get(c.Request().Context(), p.UserID)
	if err != nil {
		return err
	}
	if !u.Active {
		return httpx.ErrUnauthorized
	}
	return c.JSON(http.StatusOK, map[string]any{"user": u, "permissions": u.Role.Expand(m.knownPerm()),
		"mfa_setup_required": p.MFASetupPending})
}

// security is the person's own Security page: two-step status and where they're signed in.
func (m *Module) security(c echo.Context) error {
	ctx := c.Request().Context()
	p, _ := auth.FromContext(ctx)
	u, err := m.svc.Get(ctx, p.UserID)
	if err != nil {
		return err
	}
	left, err := m.svc.RecoveryCodesLeft(ctx, p.UserID)
	if err != nil {
		return err
	}
	sessions, err := m.svc.ListSessions(ctx, p.UserID, p.SessionID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"mfa_enabled": u.MFAEnabled, "mfa_required": u.MFARequired,
		"recovery_codes_left": left, "sessions": sessions})
}

func (m *Module) mfaSetup(c echo.Context) error {
	p, _ := auth.FromContext(c.Request().Context())
	key, uri, err := m.svc.BeginMFASetup(c.Request().Context(), p.UserID)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, map[string]string{"key": key, "uri": uri})
}

type codeReq struct {
	Code     string `json:"code"`
	Password string `json:"password"`
}

// mfaEnable confirms the first code, turns two-step on, signs out other
// (password-only) sessions and returns the recovery codes, shown only once.
func (m *Module) mfaEnable(c echo.Context) error {
	var req codeReq
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ctx := c.Request().Context()
	p, _ := auth.FromContext(ctx)
	codes, err := m.svc.EnableMFA(ctx, p.UserID, req.Code)
	if err != nil {
		return err
	}
	if err := m.svc.MarkSessionVerified(ctx, p.SessionID); err != nil {
		return err
	}
	if err := m.svc.RevokeOtherSessions(ctx, p.UserID, p.SessionID, "mfa_enabled"); err != nil {
		return err
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, map[string]any{"recovery_codes": codes})
}

func (m *Module) mfaDisable(c echo.Context) error {
	var req codeReq
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	p, _ := auth.FromContext(c.Request().Context())
	if err := m.svc.DisableMFA(c.Request().Context(), p.UserID, req.Password, req.Code); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) mfaRecoveryCodes(c echo.Context) error {
	var req codeReq
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	p, _ := auth.FromContext(c.Request().Context())
	codes, err := m.svc.RegenerateRecoveryCodes(c.Request().Context(), p.UserID, req.Code)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, map[string]any{"recovery_codes": codes})
}

// endSession signs out one of the caller's own sessions, e.g. a lost laptop.
func (m *Module) endSession(c echo.Context) error {
	sid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return httpx.ErrNotFound
	}
	p, _ := auth.FromContext(c.Request().Context())
	if err := m.svc.RevokeOwnSession(c.Request().Context(), p.UserID, sid); err != nil {
		return err
	}
	if sid == p.SessionID {
		m.tokens.ClearCookie(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// resetMFA is for a colleague who lost their phone.
func (m *Module) resetMFA(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return httpx.ErrNotFound
	}
	u, err := m.svc.ResetMFA(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, u)
}

func (m *Module) list(c echo.Context) error {
	us, err := m.svc.List(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": us})
}

// Colleague is the public part of a staff member, for pickers.
type Colleague struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Position string    `json:"position"`
}

func (m *Module) directory(c echo.Context) error {
	rows, err := m.svc.pool.Query(c.Request().Context(),
		`SELECT id, full_name, position FROM users WHERE active ORDER BY full_name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []Colleague{}
	for rows.Next() {
		var p Colleague
		if err := rows.Scan(&p.ID, &p.Name, &p.Position); err != nil {
			return err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": out})
}

// invited is returned when a set-up link was issued: who it was for and
// whether the email went out.
type invited struct {
	User User `json:"user"`
	delivery
}

func (m *Module) create(c echo.Context) error {
	var in CreateInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	ctx := c.Request().Context()
	u, tok, err := m.svc.Invite(ctx, in)
	if err != nil {
		return err
	}
	p, _ := auth.FromContext(ctx)
	return c.JSON(http.StatusCreated, invited{User: u, delivery: m.sendInvite(ctx, u, tok, p.Name)})
}

func (m *Module) resendInvite(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return httpx.ErrNotFound
	}
	ctx := c.Request().Context()
	u, tok, err := m.svc.RenewInvite(ctx, id)
	if err != nil {
		return err
	}
	p, _ := auth.FromContext(ctx)
	return c.JSON(http.StatusOK, invited{User: u, delivery: m.sendInvite(ctx, u, tok, p.Name)})
}

func (m *Module) lookupSetup(c echo.Context) error {
	l, err := m.svc.LookupSetup(c.Request().Context(), c.QueryParam("token"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, l)
}

type setupReq struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// completeSetup sets the password from an emailed link and signs the person in.
func (m *Module) completeSetup(c echo.Context) error {
	var req setupReq
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	u, err := m.svc.CompleteSetup(c.Request().Context(), req.Token, req.Password)
	if err != nil {
		return err
	}
	m.svc.Unlock(c.Request().Context(), u.Email)
	if err := m.svc.RevokeAllSessions(c.Request().Context(), u.ID, "password_changed"); err != nil {
		return err
	}
	// A set-up link proves access to the email, not the phone: two-step still applies.
	return m.afterPassword(c, u)
}

func (m *Module) update(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return httpx.ErrNotFound
	}
	var in UpdateInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	u, err := m.svc.Update(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, u)
}

func (m *Module) resetPassword(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return httpx.ErrNotFound
	}
	ctx := c.Request().Context()
	u, tok, err := m.svc.ResetPassword(ctx, id)
	if err != nil {
		return err
	}
	m.svc.Unlock(ctx, u.Email)
	if err := m.svc.RevokeAllSessions(ctx, u.ID, "password_reset"); err != nil {
		return err
	}
	p, _ := auth.FromContext(ctx)
	return c.JSON(http.StatusOK, invited{User: u, delivery: m.sendReset(ctx, u, tok, p.Name)})
}

func (m *Module) setActive(active bool) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			return httpx.ErrNotFound
		}
		u, err := m.svc.SetActive(c.Request().Context(), id, active)
		if err != nil {
			return err
		}
		if !active {
			if err := m.svc.RevokeAllSessions(c.Request().Context(), u.ID, "deactivated"); err != nil {
				return err
			}
		}
		return c.JSON(http.StatusOK, u)
	}
}

// authRateLimit allows each network address a short burst of sign-in calls,
// then about one every five seconds.
func authRateLimit() echo.MiddlewareFunc {
	return middleware.RateLimiterWithConfig(middleware.RateLimiterConfig{
		Store: middleware.NewRateLimiterMemoryStoreWithConfig(middleware.RateLimiterMemoryStoreConfig{
			Rate: rate.Every(5 * time.Second), Burst: 10, ExpiresIn: 10 * time.Minute,
		}),
		IdentifierExtractor: func(c echo.Context) (string, error) { return c.RealIP(), nil },
		DenyHandler: func(c echo.Context, _ string, _ error) error {
			return httpx.NewError(http.StatusTooManyRequests, "rate_limited", "Too many requests. Please wait a minute and try again.")
		},
		ErrorHandler: func(c echo.Context, err error) error { return httpx.ErrForbidden },
	})
}
