package clients

import (
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
	g.POST("/check-duplicates", h.checkDuplicates, auth.Require(PermCreate))
	g.GET("/:id", h.get, auth.Require(PermRead))
	g.PATCH("/:id", h.update, auth.Require(PermUpdate))
	g.POST("/:id/deactivate", h.deactivate, auth.Require(PermDeactivate))
	g.POST("/:id/reactivate", h.reactivate, auth.Require(PermDeactivate))
	g.GET("/:id/activity", h.activity, auth.Require(PermRead))
	g.POST("/:id/contacts", h.addContact, auth.Require(PermUpdate))
	g.PUT("/:id/contacts/:contactId", h.updateContact, auth.Require(PermUpdate))
	g.POST("/:id/sites", h.addSite, auth.Require(PermUpdate))

	g.GET("/follow-ups", h.myFollowUps, auth.Require(PermRead))
	g.GET("/team/people", h.teamPeople, auth.Require(PermRead))
	g.GET("/team/log", h.teamLog, auth.Require(PermRead))
	g.GET("/team/summary", h.teamSummary, auth.Require(PermRead))
	g.GET("/team/follow-ups", h.teamFollowUps, auth.Require(PermRead))
	g.GET("/:id/interactions", h.interactions, auth.Require(PermRead))
	g.POST("/:id/interactions", h.logInteraction, auth.Require(PermLog))
	g.POST("/:id/interactions/:iid/follow-up", h.changeFollowUp, auth.Require(PermLog))

	g.GET("/:id/equipment", h.equipment, auth.Require(PermRead))
	g.POST("/:id/equipment", h.addEquipment, auth.Require(PermEquipment))
	g.PUT("/:id/equipment/:eid", h.updateEquipment, auth.Require(PermEquipment))
	g.POST("/:id/equipment/:eid/remove", h.removeEquipment, auth.Require(PermEquipment))
}

func (h *handler) list(c echo.Context) error {
	f := ListFilter{Query: c.QueryParam("q"), Status: Status(c.QueryParam("status")), Page: httpx.ParsePage(c)}
	if f.Status != "" && f.Status != StatusActive && f.Status != StatusInactive {
		return httpx.FieldErrors{"status": "Use active or inactive."}.Err()
	}
	switch a := c.QueryParam("attention"); a {
	case "", "followups", "cold":
		f.Attention = a
	default:
		return httpx.FieldErrors{"attention": "Use followups or cold."}.Err()
	}
	if p, ok := auth.FromContext(c.Request().Context()); ok {
		f.Me = p.UserID
	}
	if v := c.QueryParam("owner_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return httpx.FieldErrors{"owner_id": "Invalid id."}.Err()
		}
		f.OwnerID = &id
	}
	out, err := h.svc.List(c.Request().Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, out)
}

func (h *handler) get(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	cl, err := h.svc.Get(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, cl)
}

func (h *handler) create(c echo.Context) error {
	var in CreateInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	cl, err := h.svc.Create(c.Request().Context(), in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, cl)
}

func (h *handler) checkDuplicates(c echo.Context) error {
	var in struct {
		LegalName      string  `json:"legal_name"`
		TPIN           *string `json:"tpin"`
		RegistrationNo *string `json:"registration_no"`
	}
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	dups, err := h.svc.CheckDuplicates(c.Request().Context(), in.LegalName, in.TPIN, in.RegistrationNo)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"matches": dups})
}

func (h *handler) update(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in UpdateInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	cl, err := h.svc.Update(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, cl)
}

type statusReq struct {
	Version int    `json:"version"`
	Reason  string `json:"reason"`
}

func (h *handler) deactivate(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in statusReq
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	cl, err := h.svc.Deactivate(c.Request().Context(), id, in.Version, in.Reason)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, cl)
}

func (h *handler) reactivate(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in statusReq
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	cl, err := h.svc.Reactivate(c.Request().Context(), id, in.Version)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, cl)
}

func (h *handler) activity(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	if _, err := h.svc.Get(c.Request().Context(), id); err != nil {
		return err
	}
	evs, err := h.svc.Activity(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": evs})
}

func (h *handler) addContact(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in ContactInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	ct, err := h.svc.AddContact(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, ct)
}

func (h *handler) updateContact(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	cid, err := pathID(c, "contactId")
	if err != nil {
		return err
	}
	var in struct {
		ContactInput
		Active *bool `json:"active"`
	}
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	active := in.Active == nil || *in.Active
	ct, err := h.svc.UpdateContact(c.Request().Context(), id, cid, in.ContactInput, active)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, ct)
}

func (h *handler) addSite(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in SiteInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	st, err := h.svc.AddSite(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, st)
}

func pathID(c echo.Context, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		return uuid.Nil, httpx.ErrNotFound
	}
	return id, nil
}

func (h *handler) summary(c echo.Context) error {
	p, _ := auth.FromContext(c.Request().Context())
	sum, err := h.svc.Summary(c.Request().Context(), p.UserID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, sum)
}

// ---- contact log ----

func (h *handler) myFollowUps(c echo.Context) error {
	p, _ := auth.FromContext(c.Request().Context())
	items, err := h.svc.MyFollowUps(c.Request().Context(), p.UserID, 100)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items})
}

func (h *handler) interactions(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	items, err := h.svc.Interactions(c.Request().Context(), id, 200)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items})
}

func (h *handler) logInteraction(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in InteractionInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	it, err := h.svc.LogInteraction(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, it)
}

func (h *handler) changeFollowUp(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	iid, err := pathID(c, "iid")
	if err != nil {
		return err
	}
	var in FollowUpChange
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	it, err := h.svc.ChangeFollowUp(c.Request().Context(), id, iid, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, it)
}

// ---- equipment ----

func (h *handler) equipment(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	items, err := h.svc.Equipment(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items})
}

func (h *handler) addEquipment(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in EquipmentInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	e, err := h.svc.AddEquipment(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, e)
}

func (h *handler) updateEquipment(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	eid, err := pathID(c, "eid")
	if err != nil {
		return err
	}
	var in EquipmentInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	e, err := h.svc.UpdateEquipment(c.Request().Context(), id, eid, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, e)
}

func (h *handler) removeEquipment(c echo.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	eid, err := pathID(c, "eid")
	if err != nil {
		return err
	}
	var in statusReq
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	e, err := h.svc.RemoveEquipment(c.Request().Context(), id, eid, in.Version, in.Reason)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, e)
}
