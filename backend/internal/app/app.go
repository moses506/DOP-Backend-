// Package app wires the platform and all modules into one HTTP server.
// To add a module: construct it in buildModules and nothing else changes.
package app

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"dos-bpas/internal/clients"
	"dos-bpas/internal/deals"
	"dos-bpas/internal/notifications"
	"dos-bpas/internal/orders"
	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/config"
	"dos-bpas/internal/platform/events"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/migrate"
	"dos-bpas/internal/platform/migrations"
	"dos-bpas/internal/platform/module"
	"dos-bpas/internal/platform/notify"
	"dos-bpas/internal/platform/numbering"
	"dos-bpas/internal/quotes"
	"dos-bpas/internal/sourcing"
	"dos-bpas/internal/users"
)

type App struct {
	Echo    *echo.Echo
	Modules []module.Module
	deps    module.Deps
}

func New(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) *App {
	deps := module.Deps{
		DB:      pool,
		Tokens:  auth.NewTokens(cfg.JWTSecret, cfg.AccessTTL),
		Numbers: numbering.New(cfg.Location),
		Events:  events.NewBus(log),
		Log:     log,
		Now:     time.Now,
		Mail: mail.New(mail.Config{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword,
			From: cfg.SMTPFrom, FromName: cfg.SMTPFromName,
		}, pool, log),
		AppURL:      cfg.AppURL,
		Location:    cfg.Location,
		Notify:      notify.New(pool),
		SessionIdle: cfg.SessionIdle,
		DataKey:     cfg.DataKey,
		MFARoles:    cfg.MFARoles,

		Company:               cfg.Company,
		QuoteApprovalDiscount: cfg.QuoteApprovalDiscount,
		QuoteValidDays:        cfg.QuoteValidDays,
	}
	// Session cookies travel only over HTTPS once the app is served on https.
	deps.Tokens.SecureCookie = cfg.IsProduction() || strings.HasPrefix(cfg.AppURL, "https://")
	a := &App{deps: deps}
	a.Modules = a.buildModules()
	a.Echo = a.buildHTTP(cfg, log)
	return a
}

// buildModules is the single registration point. Order matters only for
// migrations (a module's tables may reference platform tables).
func (a *App) buildModules() []module.Module {
	var mods []module.Module
	known := func() []string {
		var out []string
		for _, m := range mods {
			out = append(out, m.Permissions()...)
		}
		return append(out, "audit:read")
	}

	usersMod := users.New(a.deps, known)
	clientsMod := clients.New(a.deps)
	// Modules receive the APIs they depend on.
	dealsMod := deals.New(a.deps, clientsMod.API())
	quotesMod := quotes.New(a.deps, clientsMod.API(), dealsMod.API())

	ordersMod := orders.New(a.deps, clientsMod.API(), quotesMod.API(), dealsMod.API())
	sourcingMod := sourcing.New(a.deps, clientsMod.API(), dealsMod.API())
	mods = append(mods, usersMod, clientsMod, dealsMod, quotesMod, ordersMod, sourcingMod, notifications.New(a.deps))
	return mods
}

// StartJobs starts the background work of every module that has some
// (e.g. follow-up reminders). It stops when ctx is cancelled.
func (a *App) StartJobs(ctx context.Context) {
	for _, m := range a.Modules {
		if s, ok := m.(module.Starter); ok {
			s.Start(ctx)
		}
	}
}

func (a *App) Migrate(ctx context.Context) error {
	sources := []migrate.Source{{Module: "platform", FS: migrations.FS}}
	for _, m := range a.Modules {
		if fsys := m.Migrations(); fsys != nil {
			sources = append(sources, migrate.Source{Module: m.Name(), FS: fsys})
		}
	}
	return migrate.Run(ctx, a.deps.DB, a.deps.Log, sources)
}

func (a *App) buildHTTP(cfg config.Config, log *slog.Logger) *echo.Echo {
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	e.HTTPErrorHandler = httpx.ErrorHandler(log)
	e.IPExtractor = ipExtractor(cfg.TrustedProxies)

	e.Use(middleware.RequestID())
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit("10M"))
	e.Use(middleware.SecureWithConfig(middleware.SecureConfig{
		XSSProtection: "0", ContentTypeNosniff: "nosniff", XFrameOptions: "DENY",
		HSTSMaxAge: hsts(cfg), HSTSExcludeSubdomains: false, ReferrerPolicy: "same-origin",
		ContentSecurityPolicy: contentSecurityPolicy,
	}))
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			h := c.Response().Header()
			h.Set("Permissions-Policy", "camera=(self), geolocation=(self), microphone=(), payment=(), usb=(), interest-cohort=()")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			if strings.HasPrefix(c.Request().URL.Path, "/api/") {
				// Business data must not linger in browser or proxy caches.
				h.Set("Cache-Control", "no-store")
			}
			return next(c)
		}
	})
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins:     cfg.CORSOrigins,
		AllowHeaders:     []string{echo.HeaderAuthorization, echo.HeaderContentType, auth.CSRFHeader, auth.ActivityHeader},
		AllowMethods:     []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete},
		AllowCredentials: true,
	}))
	// A generous overall limit per address, against scraping and floods.
	e.Use(middleware.RateLimiterWithConfig(middleware.RateLimiterConfig{
		Skipper: func(c echo.Context) bool { return !strings.HasPrefix(c.Request().URL.Path, "/api/") },
		Store: middleware.NewRateLimiterMemoryStoreWithConfig(middleware.RateLimiterMemoryStoreConfig{
			Rate: 20, Burst: 120, ExpiresIn: 5 * time.Minute,
		}),
		IdentifierExtractor: func(c echo.Context) (string, error) { return c.RealIP(), nil },
		DenyHandler: func(c echo.Context, _ string, _ error) error {
			return httpx.NewError(http.StatusTooManyRequests, "rate_limited", "Too many requests. Please slow down and try again shortly.")
		},
	}))
	e.Use(requestLogger(log))
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			r := c.Request()
			c.SetRequest(r.WithContext(audit.WithRequestID(r.Context(), httpx.RequestID(c))))
			return next(c)
		}
	})

	e.GET("/healthz", func(c echo.Context) error {
		if err := a.deps.DB.Ping(c.Request().Context()); err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "db_unavailable"})
		}
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	api := e.Group("/api")
	r := module.Router{Public: api, Authed: api.Group("", a.deps.Tokens.RequireAuth())}
	for _, m := range a.Modules {
		m.Routes(r)
	}
	api.Any("/*", func(echo.Context) error { return httpx.ErrNotFound })

	// Optionally serve the built frontend from the same server (WEB_DIR=../frontend/dist).
	if dir := os.Getenv("WEB_DIR"); dir != "" {
		e.GET("/*", spa(dir))
	}
	return e
}

// contentSecurityPolicy lets the web app load only its own scripts, styles,
// fonts and images, and talk only to its own server. Injected scripts and
// framing by other sites are blocked. Inline style attributes are allowed
// because React components use them; inline scripts are not.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; worker-src 'self'; manifest-src 'self'; " +
	"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// ipExtractor decides where the client's address comes from. Behind a reverse
// proxy only that proxy's X-Forwarded-For is believed; otherwise the
// connection's own address is used, so nobody can fake theirs.
func ipExtractor(trusted string) echo.IPExtractor {
	switch strings.TrimSpace(trusted) {
	case "":
		return echo.ExtractIPDirect()
	case "private":
		return echo.ExtractIPFromXFFHeader(echo.TrustLoopback(true), echo.TrustLinkLocal(false), echo.TrustPrivateNet(true))
	}
	opts := []echo.TrustOption{echo.TrustLoopback(false), echo.TrustLinkLocal(false), echo.TrustPrivateNet(false)}
	for _, p := range strings.Split(trusted, ",") {
		p = strings.TrimSpace(p)
		if !strings.Contains(p, "/") {
			if strings.Contains(p, ":") {
				p += "/128"
			} else {
				p += "/32"
			}
		}
		if _, n, err := net.ParseCIDR(p); err == nil {
			opts = append(opts, echo.TrustIPRange(n))
		}
	}
	return echo.ExtractIPFromXFFHeader(opts...)
}

func hsts(cfg config.Config) int {
	if cfg.IsProduction() {
		return 31536000
	}
	return 0
}

func requestLogger(log *slog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)
			if err != nil {
				c.Error(err)
			}
			if strings.HasPrefix(c.Path(), "/api") {
				log.Info("http",
					"method", c.Request().Method, "path", c.Path(), "status", c.Response().Status,
					"ms", time.Since(start).Milliseconds(), "request_id", httpx.RequestID(c))
			}
			return nil
		}
	}
}

// spa serves static files and falls back to index.html for client routes.
func spa(dir string) echo.HandlerFunc {
	return func(c echo.Context) error {
		p := filepath.Join(dir, filepath.Clean("/"+c.Param("*")))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if strings.HasSuffix(p, "sw.js") || strings.HasSuffix(p, "index.html") {
				c.Response().Header().Set("Cache-Control", "no-cache")
			}
			return c.File(p)
		}
		c.Response().Header().Set("Cache-Control", "no-cache")
		return c.File(filepath.Join(dir, "index.html"))
	}
}
