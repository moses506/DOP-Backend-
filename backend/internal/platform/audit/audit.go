// Package audit writes the append-only activity log that backs every record's
// timeline (brief sections 4.1.1 and 8.2).
//
// Record must be called with the same transaction as the change it describes,
// so the log and the data can never disagree.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
)

type Entry struct {
	EntityType string         // e.g. "client"
	EntityID   uuid.UUID      //
	Action     string         // e.g. "created", "updated", "deactivated"
	Summary    string         // human-readable line for the timeline
	Changes    map[string]any // field -> {"from":x,"to":y} or snapshot
	Reason     string         // required for some actions (deactivation, overrides)
}

type Event struct {
	ID         int64           `json:"id"`
	OccurredAt time.Time       `json:"occurred_at"`
	ActorID    *uuid.UUID      `json:"actor_id"`
	ActorName  string          `json:"actor_name"`
	ActorRole  string          `json:"actor_role"`
	EntityType string          `json:"entity_type"`
	EntityID   uuid.UUID       `json:"entity_id"`
	Action     string          `json:"action"`
	Summary    string          `json:"summary"`
	Changes    json.RawMessage `json:"changes,omitempty"`
	Reason     string          `json:"reason,omitempty"`
}

type requestIDKey struct{}

// WithRequestID lets the HTTP layer attach a request id to audit entries.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func Record(ctx context.Context, q db.Querier, e Entry) error {
	var actorID *uuid.UUID
	actorName, actorRole := "system", "system"
	if p, ok := auth.FromContext(ctx); ok {
		actorID, actorName, actorRole = &p.UserID, p.Name, string(p.Role)
	}
	var changes []byte
	if len(e.Changes) > 0 {
		var err error
		if changes, err = json.Marshal(e.Changes); err != nil {
			return fmt.Errorf("audit marshal: %w", err)
		}
	}
	reqID, _ := ctx.Value(requestIDKey{}).(string)
	_, err := q.Exec(ctx, `
		INSERT INTO audit_log (actor_id, actor_name, actor_role, entity_type, entity_id,
		                       action, summary, changes, reason, request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''))`,
		actorID, actorName, actorRole, e.EntityType, e.EntityID,
		e.Action, e.Summary, changes, e.Reason, reqID)
	if err != nil {
		return fmt.Errorf("audit record: %w", err)
	}
	return nil
}

// Timeline returns the newest-first activity for one record.
func Timeline(ctx context.Context, q db.Querier, entityType string, id uuid.UUID, limit int) ([]Event, error) {
	rows, err := q.Query(ctx, `
		SELECT id, occurred_at, actor_id, actor_name, actor_role, entity_type, entity_id,
		       action, summary, changes, COALESCE(reason,'')
		FROM audit_log
		WHERE entity_type = $1 AND entity_id = $2
		ORDER BY occurred_at DESC, id DESC
		LIMIT $3`, entityType, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var ev Event
		var changes []byte
		if err := rows.Scan(&ev.ID, &ev.OccurredAt, &ev.ActorID, &ev.ActorName, &ev.ActorRole,
			&ev.EntityType, &ev.EntityID, &ev.Action, &ev.Summary, &changes, &ev.Reason); err != nil {
			return nil, err
		}
		ev.Changes = changes
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Diff builds a {"field": {"from": a, "to": b}} map for changed fields only.
func Diff(before, after map[string]any) map[string]any {
	out := map[string]any{}
	for k, b := range after {
		a := before[k]
		if fmt.Sprint(a) != fmt.Sprint(b) {
			out[k] = map[string]any{"from": a, "to": b}
		}
	}
	return out
}
