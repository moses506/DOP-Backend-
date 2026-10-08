package sourcing

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/httpx"
)

type handler struct{ svc *Service }

func (h *handler) routes(sup, src *echo.Group) {
	sup.GET("", h.listSuppliers, auth.Require(PermSuppliersRead))
	sup.POST("", h.createSupplier, auth.Require(PermSuppliersManage))
	sup.GET("/:id", h.getSupplier, auth.Require(PermSuppliersRead))
	sup.PUT("/:id", h.updateSupplier, auth.Require(PermSuppliersManage))
	sup.POST("/:id/deactivate", h.setActive(false), auth.Require(PermSuppliersManage))
	sup.POST("/:id/reactivate", h.setActive(true), auth.Require(PermSuppliersManage))
	sup.GET("/:id/activity", h.supplierActivity, auth.Require(PermSuppliersRead))

	src.GET("/rates", h.rates, auth.Require(PermRead))
	src.PUT("/rates", h.setRate, auth.Require(PermCreate))
	src.GET("", h.listRequests, auth.Require(PermRead))
	src.POST("", h.createRequest, auth.Require(PermCreate))
	src.GET("/:id", h.getRequest, auth.Require(PermRead))
	src.PUT("/:id", h.updateRequest, auth.Require(PermCreate))
	src.GET("/:id/activity", h.requestActivity, auth.Require(PermRead))
	src.POST("/:id/send", h.send, auth.Require(PermCreate))
	src.POST("/:id/invites/:inviteId/reply", h.reply, auth.Require(PermCreate))
	src.POST("/:id/decide", h.decide, auth.Require(PermCreate))
	src.POST("/:id/cancel", h.cancel, auth.Require(PermCreate))
}

func pathID(c echo.Context, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		return uuid.Nil, httpx.ErrNotFound
	}
	return id, nil
}

func (h *handler) listSuppliers(c echo.Context) error {
	p := httpx.ParsePage(c)
	f := SupplierFilter{Query: c.QueryParam("q"), Region: c.QueryParam("region"), Terms: c.QueryParam("terms"),
		Status: c.QueryParam("status"), Limit: p.Limit, Offset: p.Offset}
	if _, ok := regions[f.Region]; f.Region != "" && !ok {
		return httpx.FieldErrors{"region": "Unknown region."}.Err()
	}
	if f.Terms != "" && f.Terms != "credit" && f.Terms != "cod" {
		return httpx.FieldErrors{"terms": "Use credit or cod."}.Err()
	}
	out, err := h.svc.ListSuppliers(c.Request().Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, out)
}

func (h *handler) getSupplier(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	x, err := h.svc.GetSupplier(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, x)
}

func (h *handler) createSupplier(c echo.Context) error {
	var in SupplierInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	x, err := h.svc.CreateSupplier(c.Request().Context(), in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, x)
}

func (h *handler) updateSupplier(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in SupplierInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	x, err := h.svc.UpdateSupplier(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, x)
}

func (h *handler) setActive(active bool) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := pathID(c, "id")
		if err != nil {
			return err
		}
		var in struct {
			Version int    `json:"version"`
			Reason  string `json:"reason"`
		}
		if err := httpx.Bind(c, &in); err != nil {
			return err
		}
		x, err := h.svc.SetSupplierActive(c.Request().Context(), id, in.Version, active, in.Reason)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, x)
	}
}

func (h *handler) supplierActivity(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	evs, err := h.svc.SupplierActivity(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": evs})
}

func (h *handler) rates(c echo.Context) error {
	rs, err := h.svc.Rates(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": rs})
}

func (h *handler) setRate(c echo.Context) error {
	var in struct {
		Currency  string `json:"currency"`
		RateToZMW string `json:"rate_to_zmw"`
	}
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	r, err := h.svc.SetRate(c.Request().Context(), in.Currency, in.RateToZMW)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, r)
}

func (h *handler) listRequests(c echo.Context) error {
	p := httpx.ParsePage(c)
	f := RequestFilter{Query: c.QueryParam("q"), Status: c.QueryParam("status"), Limit: p.Limit, Offset: p.Offset}
	switch f.Status {
	case "", "open", StatusDraft, StatusSent, StatusDecided, StatusCancelled:
	default:
		return httpx.FieldErrors{"status": "Unknown status."}.Err()
	}
	if c.QueryParam("mine") == "1" {
		if pr, ok := auth.FromContext(c.Request().Context()); ok {
			f.OwnerID = &pr.UserID
		}
	}
	if v := c.QueryParam("client_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return httpx.FieldErrors{"client_id": "Invalid id."}.Err()
		}
		f.ClientID = &id
	}
	out, err := h.svc.ListRequests(c.Request().Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, out)
}

func (h *handler) getRequest(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	r, err := h.svc.GetRequest(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, r)
}

func (h *handler) createRequest(c echo.Context) error {
	var in RequestInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	r, err := h.svc.CreateRequest(c.Request().Context(), in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, r)
}

func (h *handler) updateRequest(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in RequestInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	r, err := h.svc.UpdateRequest(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, r)
}

func (h *handler) requestActivity(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	evs, err := h.svc.RequestActivity(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": evs})
}

func (h *handler) send(c echo.Context) error {
	id, err := pathID(c, "id")
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

func (h *handler) reply(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	inv, err := pathID(c, "inviteId")
	if err != nil {
		return err
	}
	var in ReplyInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	r, err := h.svc.RecordReply(c.Request().Context(), id, inv, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, r)
}

func (h *handler) decide(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in DecideInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	r, err := h.svc.Decide(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, r)
}

func (h *handler) cancel(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in struct {
		Version int    `json:"version"`
		Reason  string `json:"reason"`
	}
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	r, err := h.svc.Cancel(c.Request().Context(), id, in.Version, in.Reason)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, r)
}
