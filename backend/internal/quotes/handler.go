package quotes

import (
	"context"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/httpx"
)

type handler struct{ svc *Service }

func (h *handler) routes(g, p *echo.Group) {
	g.GET("", h.list, auth.Require(PermRead))
	g.GET("/summary", h.summary, auth.Require(PermRead))
	g.GET("/defaults", h.defaults, auth.Require(PermCreate))
	g.POST("", h.create, auth.Require(PermCreate))
	g.GET("/:id", h.get, auth.Require(PermRead))
	g.PUT("/:id", h.update, auth.Require(PermCreate))
	g.GET("/:id/pdf", h.pdf, auth.Require(PermRead))
	g.GET("/:id/activity", h.activity, auth.Require(PermRead))
	g.POST("/:id/submit", h.versioned(h.svc.Submit), auth.Require(PermCreate))
	g.POST("/:id/revise", h.versioned(h.svc.Revise), auth.Require(PermCreate))
	g.POST("/:id/approve", h.decide(true), auth.Require(PermApprove))
	g.POST("/:id/return", h.decide(false), auth.Require(PermApprove))
	g.POST("/:id/send", h.send, auth.Require(PermCreate))
	g.POST("/:id/accept", h.accept, auth.Require(PermCreate))
	g.POST("/:id/decline", h.withReason(h.svc.Decline), auth.Require(PermCreate))
	g.POST("/:id/cancel", h.withReason(h.svc.Cancel), auth.Require(PermCreate))

	p.GET("", h.products, auth.Require(PermProductsRead))
	p.POST("", h.saveProduct, auth.Require(PermProductsManage))
	p.PUT("/:id", h.saveProduct, auth.Require(PermProductsManage))
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
	case "", "open", "draft", "pending_approval", "ready", "sent", "accepted", "declined", "expired", "superseded", "cancelled":
	default:
		return httpx.FieldErrors{"status": "Unknown status."}.Err()
	}
	for _, x := range []struct {
		key string
		dst **uuid.UUID
	}{{"client_id", &f.ClientID}, {"deal_id", &f.DealID}, {"owner_id", &f.OwnerID}} {
		if v := c.QueryParam(x.key); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				return httpx.FieldErrors{x.key: "Invalid id."}.Err()
			}
			*x.dst = &id
		}
	}
	if c.QueryParam("mine") == "1" {
		p, _ := auth.FromContext(c.Request().Context())
		f.OwnerID = &p.UserID
	}
	out, err := h.svc.List(c.Request().Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, out)
}

func (h *handler) summary(c echo.Context) error {
	out, err := h.svc.Summary(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, out)
}

// defaults are the starting values for a new quotation for a client.
func (h *handler) defaults(c echo.Context) error {
	id, err := uuid.Parse(c.QueryParam("client_id"))
	if err != nil {
		return httpx.FieldErrors{"client_id": "Choose a client."}.Err()
	}
	cl, err := h.svc.clients.Get(c.Request().Context(), id)
	if err != nil {
		return err
	}
	t := h.svc.DefaultTerms(cl.CreditTermsDays)
	return c.JSON(http.StatusOK, map[string]any{
		"currency":          cl.DefaultCurrency,
		"valid_until":       h.svc.today().AddDate(0, 0, h.svc.validDays).Format("2006-01-02"),
		"delivery_terms":    t.Delivery,
		"payment_terms":     t.Payment,
		"warranty_terms":    t.Warranty,
		"approval_discount": h.svc.ApprovalLimit(),
	})
}

func (h *handler) get(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	q, err := h.svc.Get(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, q)
}

func (h *handler) create(c echo.Context) error {
	var in QuoteInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	q, err := h.svc.Create(c.Request().Context(), in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, q)
}

func (h *handler) update(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in QuoteInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	q, err := h.svc.Update(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, q)
}

func (h *handler) pdf(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	b, name, err := h.svc.PDF(c.Request().Context(), id)
	if err != nil {
		return err
	}
	disp := "inline"
	if c.QueryParam("download") == "1" {
		disp = "attachment"
	}
	c.Response().Header().Set("Content-Disposition", disp+`; filename="`+name+`"`)
	return c.Blob(http.StatusOK, "application/pdf", b)
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

type versionReq struct {
	Version int    `json:"version"`
	Note    string `json:"note"`
	Reason  string `json:"reason"`
}

func (h *handler) versioned(fn func(ctx context.Context, id uuid.UUID, version int) (Quote, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := pathID(c)
		if err != nil {
			return err
		}
		var in versionReq
		if err := httpx.Bind(c, &in); err != nil {
			return err
		}
		q, err := fn(c.Request().Context(), id, in.Version)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, q)
	}
}

func (h *handler) withReason(fn func(ctx context.Context, id uuid.UUID, version int, reason string) (Quote, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := pathID(c)
		if err != nil {
			return err
		}
		var in versionReq
		if err := httpx.Bind(c, &in); err != nil {
			return err
		}
		q, err := fn(c.Request().Context(), id, in.Version, in.Reason)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, q)
	}
}

func (h *handler) decide(approve bool) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := pathID(c)
		if err != nil {
			return err
		}
		var in versionReq
		if err := httpx.Bind(c, &in); err != nil {
			return err
		}
		q, err := h.svc.Decide(c.Request().Context(), id, in.Version, approve, in.Note)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, q)
	}
}

func (h *handler) send(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in SendInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	out, err := h.svc.Send(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, out)
}

func (h *handler) accept(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in struct {
		Version  int    `json:"version"`
		ClientPO string `json:"client_po"`
		Note     string `json:"note"`
	}
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	q, err := h.svc.Accept(c.Request().Context(), id, in.Version, in.ClientPO, in.Note)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, q)
}

func (h *handler) products(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	items, err := h.svc.Products(c.Request().Context(), c.QueryParam("q"), c.QueryParam("all") == "1", limit)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items})
}

func (h *handler) saveProduct(c echo.Context) error {
	var in ProductInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	var id *uuid.UUID
	status := http.StatusCreated
	if c.Param("id") != "" {
		pid, err := pathID(c)
		if err != nil {
			return err
		}
		id, status = &pid, http.StatusOK
	}
	p, err := h.svc.SaveProduct(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(status, p)
}
