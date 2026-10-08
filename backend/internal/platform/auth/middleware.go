package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"dos-bpas/internal/platform/httpx"
)

// The web app's session lives in an HttpOnly cookie, so page scripts (and
// anything injected into them) can't read it. SameSite=Strict stops other
// sites sending it, and CSRFHeader is a second guard on changes.
const (
	CookieName = "dos_session"
	// CSRFHeader must accompany every state-changing request authenticated by
	// the cookie. Browsers won't let another site add it without our CORS approval.
	CSRFHeader = "X-Requested-With"
	CSRFValue  = "dos-web"
	// ActivityHeader is "0" on requests the app makes on its own while the person
	// is away, so polling doesn't keep an idle session alive.
	ActivityHeader = "X-User-Active"
)

var errCSRF = httpx.NewError(http.StatusForbidden, "csrf", "This request was blocked for your security. Reload the page and try again.")

// SetCookie stores the session token in the browser.
func (t *Tokens) SetCookie(c echo.Context, token string, expires time.Time) {
	c.SetCookie(&http.Cookie{
		Name: CookieName, Value: token, Path: "/api", Expires: expires,
		MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true, Secure: t.SecureCookie, SameSite: http.SameSiteStrictMode,
	})
}

// ClearCookie removes the session cookie.
func (t *Tokens) ClearCookie(c echo.Context) {
	c.SetCookie(&http.Cookie{Name: CookieName, Value: "", Path: "/api", MaxAge: -1, HttpOnly: true, Secure: t.SecureCookie, SameSite: http.SameSiteStrictMode})
}

// RequireAuth accepts the session cookie (the web app) or a bearer token
// (scripts and integrations), confirms the session is still open, and stores
// the Principal on the request context.
func (t *Tokens) RequireAuth() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			raw, fromCookie := "", false
			if h, ok := strings.CutPrefix(req.Header.Get(echo.HeaderAuthorization), "Bearer "); ok && h != "" {
				raw = h
			} else if ck, err := req.Cookie(CookieName); err == nil && ck.Value != "" {
				raw, fromCookie = ck.Value, true
			}
			if raw == "" {
				return httpx.ErrUnauthorized
			}
			if fromCookie && !safeMethod(req.Method) && req.Header.Get(CSRFHeader) != CSRFValue {
				return errCSRF
			}
			p, err := t.Parse(raw)
			if err != nil {
				if fromCookie {
					t.ClearCookie(c)
				}
				return httpx.ErrUnauthorized
			}
			p.Active = req.Header.Get(ActivityHeader) != "0"
			if t.check != nil {
				if err := t.check(req.Context(), &p); err != nil {
					if errors.Is(err, ErrSessionRevoked) {
						if fromCookie {
							t.ClearCookie(c)
						}
						return httpx.ErrUnauthorized
					}
					return err
				}
			}
			if p.MFASetupPending && !allowedBeforeMFA(req.URL.Path) {
				return errMFASetup
			}
			c.SetRequest(req.WithContext(WithPrincipal(req.Context(), p)))
			return next(c)
		}
	}
}

var errMFASetup = httpx.NewError(http.StatusForbidden, "mfa_setup_required",
	"Set up two-step verification to continue. It's required for your role.")

// allowedBeforeMFA lists what someone may do before finishing a required
// two-step verification set-up.
func allowedBeforeMFA(path string) bool {
	switch path {
	case "/api/auth/me", "/api/auth/logout", "/api/auth/logout-all", "/api/auth/change-password",
		"/api/auth/mfa/setup", "/api/auth/mfa/enable":
		return true
	}
	return false
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// Require rejects the request unless the user's role grants perm.
func Require(perm string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			p, ok := FromContext(c.Request().Context())
			if !ok {
				return httpx.ErrUnauthorized
			}
			if !p.Can(perm) {
				return httpx.ErrForbidden
			}
			return next(c)
		}
	}
}
