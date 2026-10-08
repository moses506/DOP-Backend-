package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"dos-bpas/internal/platform/httpx"
)

func TestCookieAuthNeedsCSRFHeaderForChanges(t *testing.T) {
	tokens := NewTokens([]byte("0123456789abcdef0123456789abcdef0123456789abcdef"), time.Hour)
	tok, _, err := tokens.Issue(Principal{UserID: uuid.New(), Name: "T", Role: RoleAdmin, SessionID: uuid.New()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	e.HTTPErrorHandler = httpx.ErrorHandler(nil)
	ok := func(c echo.Context) error { return c.NoContent(http.StatusNoContent) }
	e.GET("/api/x", ok, tokens.RequireAuth())
	e.POST("/api/x", ok, tokens.RequireAuth())

	do := func(method string, header map[string]string, cookie bool) int {
		req := httptest.NewRequest(method, "/api/x", nil)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		if cookie {
			req.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do(http.MethodGet, nil, true); c != http.StatusNoContent {
		t.Fatalf("GET with cookie = %d", c)
	}
	if c := do(http.MethodPost, nil, true); c != http.StatusForbidden {
		t.Fatalf("POST with cookie, no CSRF header = %d, want 403", c)
	}
	if c := do(http.MethodPost, map[string]string{CSRFHeader: CSRFValue}, true); c != http.StatusNoContent {
		t.Fatalf("POST with cookie and header = %d", c)
	}
	if c := do(http.MethodPost, map[string]string{"Authorization": "Bearer " + tok}, false); c != http.StatusNoContent {
		t.Fatalf("POST with bearer token = %d", c)
	}
	if c := do(http.MethodGet, nil, false); c != http.StatusUnauthorized {
		t.Fatalf("no credentials = %d", c)
	}
	forged, _, _ := NewTokens([]byte("another-secret-another-secret-another-secret!!"), time.Hour).Issue(Principal{UserID: uuid.New(), Role: RoleAdmin}, time.Now())
	if c := do(http.MethodGet, map[string]string{"Authorization": "Bearer " + forged}, false); c != http.StatusUnauthorized {
		t.Fatalf("token signed with another key = %d", c)
	}
}

func TestSetupOnlySessionIsLimited(t *testing.T) {
	tokens := NewTokens([]byte("0123456789abcdef0123456789abcdef0123456789abcdef"), time.Hour)
	tokens.SetCheck(func(_ context.Context, p *Principal) error { p.MFASetupPending = true; return nil })
	tok, _, _ := tokens.Issue(Principal{UserID: uuid.New(), Role: RoleAdmin, SessionID: uuid.New()}, time.Now())
	e := echo.New()
	e.HTTPErrorHandler = httpx.ErrorHandler(nil)
	ok := func(c echo.Context) error { return c.NoContent(http.StatusNoContent) }
	for _, p := range []string{"/api/clients", "/api/auth/me", "/api/auth/mfa/setup"} {
		e.GET(p, ok, tokens.RequireAuth())
	}
	get := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := get("/api/clients"); c != http.StatusForbidden {
		t.Fatalf("business data before two-step set-up = %d, want 403", c)
	}
	if c := get("/api/auth/mfa/setup"); c != http.StatusNoContent {
		t.Fatalf("set-up route = %d", c)
	}
	if c := get("/api/auth/me"); c != http.StatusNoContent {
		t.Fatalf("me = %d", c)
	}
}
