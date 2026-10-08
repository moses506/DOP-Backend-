package orders

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/httpx"
)

type handler struct{ svc *Service }

func (h *handler) routes(g *echo.Group) {
	g.GET("", h.list, auth.Require(PermRead))
	g.GET("/summary", h.summary, auth.Require(PermRead))
	g.POST("", h.create, auth.Require(PermCreate))
	g.GET("/:id", h.get, auth.Require(PermRead))
	g.GET("/:id/activity", h.activity, auth.Require(PermRead))
	g.PATCH("/:id", h.update, auth.Require(PermRead)) // owner or manager, checked in the service
	g.POST("/:id/progress", h.progress, auth.Require(PermRead))
	g.POST("/:id/deliveries", h.deliver, auth.Require(PermRead))
	g.POST("/:id/hold", h.withReason(h.svc.Hold), auth.Require(PermRead))
	g.POST("/:id/resume", h.versioned(h.svc.Resume), auth.Require(PermRead))
	g.POST("/:id/cancel", h.withReason(h.svc.Cancel), auth.Require(PermRead))
	g.POST("/:id/invoiced", h.invoiced, auth.Require(PermRead))
}

func pathID(c echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return uuid.Nil, httpx.ErrNotFound
	}
	return id, nil
}

func (h *handler) list(c echo.Context) error {
	page := httpx.ParsePage(c)
	f := ListFilter{Query: c.QueryParam("q"), Status: c.QueryParam("status"), Limit: page.Limit, Offset: page.Offset}
	switch f.Status {
	case "", "live", "late", "open", "in_progress", "ready", "part_delivered", "delivered", "invoiced", "on_hold", "cancelled":
	default:
		return httpx.FieldErrors{"status": "Unknown status."}.Err()
	}
	for _, x := range []struct {
		key string
		dst **uuid.UUID
	}{{"client_id", &f.ClientID}, {"quote_id", &f.QuoteID}, {"deal_id", &f.DealID}, {"owner_id", &f.OwnerID}} {
		if v := c.QueryParam(x.key); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				return httpx.FieldErrors{x.key: "Invalid id."}.Err()
			}
			*x.dst = &id
		}
	}
	if c.QueryParam("mine") == "1" {
		if p, ok := auth.FromContext(c.Request().Context()); ok {
			f.OwnerID = &p.UserID
		}
	}
	out, err := h.svc.List(c.Request().Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, out)
}

func (h *handler) summary(c echo.Context) error {
	s, err := h.svc.Summary(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s)
}

func (h *handler) get(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	o, err := h.svc.Get(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, o)
}

func (h *handler) activity(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	evs, err := h.svc.Activity(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": evs})
}

func (h *handler) create(c echo.Context) error {
	var in CreateInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	o, err := h.svc.Create(c.Request().Context(), in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, o)
}

func (h *handler) update(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in UpdateInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	o, err := h.svc.Update(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, o)
}

func (h *handler) progress(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in ProgressInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	o, err := h.svc.Progress(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, o)
}

func (h *handler) deliver(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in DeliveryInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	o, err := h.svc.RecordDelivery(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, o)
}

func (h *handler) invoiced(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in struct {
		Version    int    `json:"version"`
		InvoiceRef string `json:"invoice_ref"`
		InvoicedOn string `json:"invoiced_on"`
	}
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	o, err := h.svc.MarkInvoiced(c.Request().Context(), id, in.Version, in.InvoiceRef, in.InvoicedOn)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, o)
}

type versionReq struct {
	Version int    `json:"version"`
	Reason  string `json:"reason"`
}

func (h *handler) versioned(fn func(ctx context.Context, id uuid.UUID, version int) (Order, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := pathID(c)
		if err != nil {
			return err
		}
		var in versionReq
		if err := httpx.Bind(c, &in); err != nil {
			return err
		}
		o, err := fn(c.Request().Context(), id, in.Version)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, o)
	}
}

func (h *handler) withReason(fn func(ctx context.Context, id uuid.UUID, version int, reason string) (Order, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := pathID(c)
		if err != nil {
			return err
		}
		var in versionReq
		if err := httpx.Bind(c, &in); err != nil {
			return err
		}
		o, err := fn(c.Request().Context(), id, in.Version, in.Reason)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, o)
	}
}
