package deals

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
	g.GET("/options", h.options, auth.Require(PermRead))
	g.POST("", h.create, auth.Require(PermCreate))
	g.GET("/:id", h.get, auth.Require(PermRead))
	g.PATCH("/:id", h.update, auth.Require(PermUpdate))
	g.POST("/:id/stage", h.move, auth.Require(PermUpdate))
	g.GET("/:id/notes", h.notes, auth.Require(PermRead))
	g.POST("/:id/notes", h.addNote, auth.Require(PermUpdate))
	g.GET("/:id/activity", h.activity, auth.Require(PermRead))
}

func (h *handler) list(c echo.Context) error {
	f := ListFilter{Query: c.QueryParam("q"), Stage: c.QueryParam("stage"), Page: httpx.ParsePage(c)}
	if f.Stage != "" && f.Stage != "open" && f.Stage != "closed" && !Stage(f.Stage).Valid() {
		return httpx.FieldErrors{"stage": "Use open, closed or a stage name."}.Err()
	}
	var err error
	if f.OwnerID, err = queryID(c, "owner_id"); err != nil {
		return err
	}
	if c.QueryParam("mine") == "1" {
		p, _ := auth.FromContext(c.Request().Context())
		f.OwnerID = &p.UserID
	}
	if f.ClientID, err = queryID(c, "client_id"); err != nil {
		return err
	}
	out, err := h.svc.List(c.Request().Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, out)
}

func (h *handler) summary(c echo.Context) error {
	p, _ := auth.FromContext(c.Request().Context())
	var owner *uuid.UUID
	if c.QueryParam("mine") == "1" {
		owner = &p.UserID
	}
	sum, err := h.svc.Summary(c.Request().Context(), p.UserID, owner)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, sum)
}

// options lists the stages (with default probabilities), note kinds and source suggestions.
func (h *handler) options(c echo.Context) error {
	type stage struct {
		Value       Stage  `json:"value"`
		Label       string `json:"label"`
		Probability int    `json:"probability"`
	}
	st := make([]stage, 0, len(Stages))
	for _, s := range Stages {
		st = append(st, stage{s, stageLabel(s), s.DefaultProbability()})
	}
	return c.JSON(http.StatusOK, map[string]any{"stages": st, "note_kinds": NoteKinds, "sources": Sources})
}

func (h *handler) get(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	d, err := h.svc.Get(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, d)
}

func (h *handler) create(c echo.Context) error {
	var in CreateInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	d, err := h.svc.Create(c.Request().Context(), in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, d)
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
	d, err := h.svc.Update(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, d)
}

func (h *handler) move(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in MoveInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	d, err := h.svc.Move(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, d)
}

func (h *handler) notes(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	ns, err := h.svc.Notes(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": ns})
}

func (h *handler) addNote(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in NoteInput
	if err := httpx.Bind(c, &in); err != nil {
		return err
	}
	n, err := h.svc.AddNote(c.Request().Context(), id, in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, n)
}

func (h *handler) activity(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	ev, err := h.svc.Activity(c.Request().Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": ev})
}

func pathID(c echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return uuid.Nil, httpx.ErrNotFound
	}
	return id, nil
}

func queryID(c echo.Context, name string) (*uuid.UUID, error) {
	v := c.QueryParam(name)
	if v == "" {
		return nil, nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil, httpx.FieldErrors{name: "Invalid id."}.Err()
	}
	return &id, nil
}
