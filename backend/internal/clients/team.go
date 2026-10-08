package clients

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/httpx"
)

// Team activity.
//
// Sales executives report to a manager (users.manager_id). A manager sees the
// contact log of everyone below them, however deep, and their own. People who
// can manage clients (sales managers, the CEO, admins) can widen that to the
// whole company. Everyone else sees just themselves.

// TeamScope picks whose activity to show.
type TeamScope string

const (
	ScopeTeam TeamScope = "team" // me and everyone who reports to me
	ScopeAll  TeamScope = "all"  // everyone; needs clients:manage
)

// TeamMember is one person on the activity page.
type TeamMember struct {
	Person
	Role      string     `json:"role"`
	ManagerID *uuid.UUID `json:"manager_id"`
	IsMe      bool       `json:"is_me"`
	Direct    bool       `json:"direct"` // reports straight to me
}

// MemberSummary is what one person did in the period.
type MemberSummary struct {
	Person        Person         `json:"person"`
	Counts        map[string]int `json:"counts"` // by kind
	Total         int            `json:"total"`  // contacts with clients (notes excluded)
	CheckedIn     int            `json:"checked_in"`
	Clients       int            `json:"clients"` // distinct clients contacted
	Overdue       int            `json:"follow_ups_overdue"`
	DueToday      int            `json:"follow_ups_due_today"`
	LastActivity  *time.Time     `json:"last_activity"` // latest entry ever, not just in the period
	FollowUpsSet  int            `json:"follow_ups_set"`
	FollowUpsDone int            `json:"follow_ups_done"`
}

type TeamFilter struct {
	Scope  TeamScope
	UserID *uuid.UUID
	Kind   string
	From   time.Time // first day, inclusive (UTC midnight of the local date)
	To     time.Time // last day, inclusive
	Limit  int
	Offset int
}

// visibleIDs returns whose activity the actor may see for the scope.
func (s *Service) visibleIDs(ctx context.Context, actor auth.Principal, scope TeamScope) ([]uuid.UUID, error) {
	if scope == ScopeAll {
		if !actor.Can(PermManage) {
			return nil, httpx.ErrForbidden
		}
		rows, err := s.pool.Query(ctx, `SELECT id FROM users WHERE active OR id IN (SELECT DISTINCT created_by FROM client_interactions)`)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	}
	return teamIDs(ctx, s.pool, actor.UserID)
}

// teamIDs is the person and everyone below them in the reporting tree.
func teamIDs(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, root uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE t(id, depth) AS (
			SELECT $1::uuid, 0
			UNION
			SELECT u.id, t.depth + 1 FROM users u JOIN t ON u.manager_id = t.id WHERE t.depth < 20
		)
		SELECT DISTINCT id FROM t`, root)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// TeamPeople lists the people the actor can see, for the filter and tiles.
func (s *Service) TeamPeople(ctx context.Context, actor auth.Principal, scope TeamScope) ([]TeamMember, error) {
	ids, err := s.visibleIDs(ctx, actor, scope)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, full_name, position, role, manager_id
		FROM users
		WHERE id = ANY($1) AND (active OR id = $2 OR EXISTS (SELECT 1 FROM client_interactions i WHERE i.created_by = users.id))
		ORDER BY (id = $2) DESC, full_name`, ids, actor.UserID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TeamMember, error) {
		var m TeamMember
		err := r.Scan(&m.ID, &m.Name, &m.Position, &m.Role, &m.ManagerID)
		m.IsMe = m.ID == actor.UserID
		m.Direct = m.ManagerID != nil && *m.ManagerID == actor.UserID
		return m, err
	})
}

// range bounds as instants in the business timezone.
func (s *Service) bounds(from, to time.Time) (time.Time, time.Time) {
	l := s.loc()
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, l)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, l).AddDate(0, 0, 1)
	return a, b
}

func (s *Service) teamWhere(ctx context.Context, actor auth.Principal, f TeamFilter) (string, []any, error) {
	ids, err := s.visibleIDs(ctx, actor, f.Scope)
	if err != nil {
		return "", nil, err
	}
	if f.UserID != nil {
		ok := false
		for _, id := range ids {
			if id == *f.UserID {
				ok = true
				break
			}
		}
		if !ok {
			return "", nil, httpx.ErrForbidden
		}
		ids = []uuid.UUID{*f.UserID}
	}
	a, b := s.bounds(f.From, f.To)
	where := "i.created_by = ANY($1) AND i.occurred_at >= $2 AND i.occurred_at < $3"
	args := []any{ids, a, b}
	if f.Kind != "" {
		args = append(args, f.Kind)
		where += fmt.Sprintf(" AND i.kind = $%d", len(args))
	}
	return where, args, nil
}

// TeamLog is the feed of entries for the people and period.
func (s *Service) TeamLog(ctx context.Context, actor auth.Principal, f TeamFilter) ([]Interaction, bool, error) {
	where, args, err := s.teamWhere(ctx, actor, f)
	if err != nil {
		return nil, false, err
	}
	items, err := queryInteractions(ctx, s.pool, where, args, f.Limit+1, f.Offset, true)
	if err != nil {
		return nil, false, err
	}
	more := len(items) > f.Limit
	if more {
		items = items[:f.Limit]
	}
	return items, more, nil
}

// TeamSummary is one row per visible person (or just the chosen one).
func (s *Service) TeamSummary(ctx context.Context, actor auth.Principal, f TeamFilter) ([]MemberSummary, error) {
	people, err := s.TeamPeople(ctx, actor, f.Scope)
	if err != nil {
		return nil, err
	}
	if f.UserID != nil {
		var keep []TeamMember
		for _, p := range people {
			if p.ID == *f.UserID {
				keep = append(keep, p)
			}
		}
		if keep == nil {
			return nil, httpx.ErrForbidden
		}
		people = keep
	}
	ids := make([]uuid.UUID, len(people))
	out := make([]MemberSummary, len(people))
	idx := map[uuid.UUID]int{}
	for i, p := range people {
		ids[i] = p.ID
		idx[p.ID] = i
		out[i] = MemberSummary{Person: p.Person, Counts: map[string]int{}}
	}
	a, b := s.bounds(f.From, f.To)

	rows, err := s.pool.Query(ctx, `
		SELECT created_by, kind, count(*), count(*) FILTER (WHERE checked_in_at IS NOT NULL),
		       count(*) FILTER (WHERE follow_up_on IS NOT NULL)
		FROM client_interactions
		WHERE created_by = ANY($1) AND occurred_at >= $2 AND occurred_at < $3
		GROUP BY created_by, kind`, ids, a, b)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		var kind string
		var n, ci, fs int
		if err := rows.Scan(&id, &kind, &n, &ci, &fs); err != nil {
			rows.Close()
			return nil, err
		}
		m := &out[idx[id]]
		m.Counts[kind] += n
		if kind != "note" {
			m.Total += n
		}
		m.CheckedIn += ci
		m.FollowUpsSet += fs
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	today := s.today().Format("2006-01-02")
	rows, err = s.pool.Query(ctx, `
		SELECT u.id,
		       (SELECT count(DISTINCT client_id) FROM client_interactions
		         WHERE created_by = u.id AND kind <> 'note' AND occurred_at >= $2 AND occurred_at < $3),
		       (SELECT count(*) FROM client_interactions
		         WHERE follow_up_owner_id = u.id AND follow_up_done_at IS NULL AND follow_up_on < $4::date),
		       (SELECT count(*) FROM client_interactions
		         WHERE follow_up_owner_id = u.id AND follow_up_done_at IS NULL AND follow_up_on = $4::date),
		       (SELECT count(*) FROM client_interactions
		         WHERE follow_up_done_by = u.id AND follow_up_done_at >= $2 AND follow_up_done_at < $3),
		       (SELECT max(occurred_at) FROM client_interactions WHERE created_by = u.id)
		FROM users u WHERE u.id = ANY($1)`, ids, a, b, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var cl, od, dt, done int
		var last *time.Time
		if err := rows.Scan(&id, &cl, &od, &dt, &done, &last); err != nil {
			return nil, err
		}
		m := &out[idx[id]]
		m.Clients, m.Overdue, m.DueToday, m.FollowUpsDone, m.LastActivity = cl, od, dt, done, last
	}
	return out, rows.Err()
}

// ---- HTTP ----

func (h *handler) teamFilter(c echo.Context) (TeamFilter, error) {
	fe := httpx.FieldErrors{}
	f := TeamFilter{Scope: ScopeTeam, Limit: 50}
	switch v := TeamScope(c.QueryParam("scope")); v {
	case "", ScopeTeam:
	case ScopeAll:
		f.Scope = ScopeAll
	default:
		fe.Add("scope", "Use team or all.")
	}
	if v := c.QueryParam("user_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			fe.Add("user_id", "Invalid id.")
		} else {
			f.UserID = &id
		}
	}
	if v := c.QueryParam("kind"); v != "" {
		if _, ok := interactionKinds[v]; !ok {
			fe.Add("kind", "Unknown kind.")
		}
		f.Kind = v
	}
	today := h.svc.today()
	f.From, f.To = today, today
	if v := c.QueryParam("from"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			fe.Add("from", "Use YYYY-MM-DD.")
		}
		f.From = t
	}
	if v := c.QueryParam("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			fe.Add("to", "Use YYYY-MM-DD.")
		}
		f.To = t
	}
	if f.To.Before(f.From) {
		fe.Add("to", "The end date is before the start date.")
	} else if f.To.Sub(f.From) > 366*24*time.Hour {
		fe.Add("to", "Pick a period of a year or less.")
	}
	if v, err := strconv.Atoi(c.QueryParam("limit")); err == nil && v > 0 && v <= 200 {
		f.Limit = v
	}
	if v, err := strconv.Atoi(c.QueryParam("offset")); err == nil && v >= 0 {
		f.Offset = v
	}
	return f, fe.Err()
}

func (h *handler) teamPeople(c echo.Context) error {
	p, _ := auth.FromContext(c.Request().Context())
	scope := ScopeTeam
	if c.QueryParam("scope") == string(ScopeAll) {
		scope = ScopeAll
	}
	ctx := c.Request().Context()
	items, err := h.svc.TeamPeople(ctx, p, scope)
	if err != nil {
		return err
	}
	// Where the page should open: people with a team see their team; people
	// who can see everyone but have nobody reporting to them (admins, the
	// CEO before reporting lines are set) see everyone, not an empty page.
	def := ScopeTeam
	if p.Can(PermManage) {
		ids, err := teamIDs(ctx, h.svc.pool, p.UserID)
		if err != nil {
			return err
		}
		if len(ids) <= 1 {
			def = ScopeAll
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items, "can_see_all": p.Can(PermManage), "default_scope": def})
}

// TeamFollowUps are the open follow-ups of the visible people that are overdue
// or due within the next week — the work still waiting, not just what was logged.
func (s *Service) TeamFollowUps(ctx context.Context, actor auth.Principal, f TeamFilter) ([]Interaction, error) {
	ids, err := s.visibleIDs(ctx, actor, f.Scope)
	if err != nil {
		return nil, err
	}
	if f.UserID != nil {
		ok := false
		for _, id := range ids {
			if id == *f.UserID {
				ok = true
				break
			}
		}
		if !ok {
			return nil, httpx.ErrForbidden
		}
		ids = []uuid.UUID{*f.UserID}
	}
	horizon := s.today().AddDate(0, 0, 7).Format("2006-01-02")
	items, err := queryInteractions(ctx, s.pool,
		`i.follow_up_owner_id = ANY($1) AND i.follow_up_on IS NOT NULL AND i.follow_up_done_at IS NULL
		 AND i.follow_up_on <= $2::date AND cl.status = 'active'`,
		[]any{ids, horizon}, 200, 0, true)
	if err != nil {
		return nil, err
	}
	// Soonest (most overdue) first.
	sort.SliceStable(items, func(a, b int) bool { return items[a].FollowUp.On < items[b].FollowUp.On })
	return items, nil
}

func (h *handler) teamFollowUps(c echo.Context) error {
	f, err := h.teamFilter(c)
	if err != nil {
		return err
	}
	p, _ := auth.FromContext(c.Request().Context())
	items, err := h.svc.TeamFollowUps(c.Request().Context(), p, f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items})
}

func (h *handler) teamLog(c echo.Context) error {
	f, err := h.teamFilter(c)
	if err != nil {
		return err
	}
	p, _ := auth.FromContext(c.Request().Context())
	items, more, err := h.svc.TeamLog(c.Request().Context(), p, f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items, "more": more})
}

func (h *handler) teamSummary(c echo.Context) error {
	f, err := h.teamFilter(c)
	if err != nil {
		return err
	}
	p, _ := auth.FromContext(c.Request().Context())
	items, err := h.svc.TeamSummary(c.Request().Context(), p, f)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items})
}
