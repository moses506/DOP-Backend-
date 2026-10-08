// Package notifications serves the bell in the top bar: each person lists and
// dismisses their own notifications. Other modules create them through
// platform/notify.
package notifications

import (
	"io/fs"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/module"
	"dos-bpas/internal/platform/notify"
)

type Module struct{ svc *notify.Service }

func New(d module.Deps) *Module { return &Module{svc: d.Notify} }

func (m *Module) Name() string          { return "notifications" }
func (m *Module) Migrations() fs.FS     { return nil } // tables live in the platform migrations
func (m *Module) Permissions() []string { return nil } // everyone sees only their own

func (m *Module) Routes(r module.Router) {
	g := r.Authed.Group("/notifications")
	g.GET("", m.list)
	g.POST("/read-all", m.readAll)
	g.POST("/:id/read", m.read)
}

func (m *Module) list(c echo.Context) error {
	p, ok := auth.FromContext(c.Request().Context())
	if !ok {
		return httpx.ErrUnauthorized
	}
	items, unread, err := m.svc.List(c.Request().Context(), p.UserID, 30)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items, "unread": unread})
}

func (m *Module) read(c echo.Context) error {
	p, ok := auth.FromContext(c.Request().Context())
	if !ok {
		return httpx.ErrUnauthorized
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return httpx.ErrNotFound
	}
	if err := m.svc.MarkRead(c.Request().Context(), p.UserID, id); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) readAll(c echo.Context) error {
	p, ok := auth.FromContext(c.Request().Context())
	if !ok {
		return httpx.ErrUnauthorized
	}
	if err := m.svc.MarkAllRead(c.Request().Context(), p.UserID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
