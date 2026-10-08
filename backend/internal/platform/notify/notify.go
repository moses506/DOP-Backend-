// Package notify stores in-app notifications (the bell in the top bar).
// Modules create them, usually inside the transaction that caused them;
// people read and dismiss their own through the notifications routes.
package notify

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/platform/db"
)

type Notification struct {
	ID         uuid.UUID  `json:"id"`
	UserID     uuid.UUID  `json:"-"`
	Kind       string     `json:"kind"`
	Title      string     `json:"title"`
	Body       string     `json:"body,omitempty"`
	Link       string     `json:"link,omitempty"`
	EntityType string     `json:"entity_type,omitempty"`
	EntityID   *uuid.UUID `json:"entity_id,omitempty"`
	// DedupeKey, when set, makes Create a no-op if the same key was used before.
	DedupeKey string     `json:"-"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at"`
}

type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Create stores a notification and reports whether it was new.
// Pass a transaction to tie it to the change that caused it.
func Create(ctx context.Context, q db.Querier, n Notification) (bool, error) {
	var key, body, link, et *string
	if n.DedupeKey != "" {
		key = &n.DedupeKey
	}
	if n.Body != "" {
		body = &n.Body
	}
	if n.Link != "" {
		link = &n.Link
	}
	if n.EntityType != "" {
		et = &n.EntityType
	}
	tag, err := q.Exec(ctx, `
		INSERT INTO notifications (user_id, kind, title, body, link, entity_type, entity_id, dedupe_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (dedupe_key) DO NOTHING`,
		n.UserID, n.Kind, n.Title, body, link, et, n.EntityID, key)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Service) Create(ctx context.Context, n Notification) (bool, error) {
	return Create(ctx, s.pool, n)
}

// List returns a person's most recent notifications and how many are unread.
func (s *Service) List(ctx context.Context, userID uuid.UUID, limit int) ([]Notification, int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, title, coalesce(body,''), coalesce(link,''), coalesce(entity_type,''), entity_id, created_at, read_at
		FROM notifications WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.Link, &n.EntityType, &n.EntityID, &n.CreatedAt, &n.ReadAt); err != nil {
			return nil, 0, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var unread int
	err = s.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, userID).Scan(&unread)
	return out, unread, err
}

// MarkRead marks one of the person's notifications as read.
func (s *Service) MarkRead(ctx context.Context, userID, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE notifications SET read_at = now() WHERE id = $1 AND user_id = $2 AND read_at IS NULL`, id, userID)
	return err
}

// MarkAllRead marks every unread notification of the person as read.
func (s *Service) MarkAllRead(ctx context.Context, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE notifications SET read_at = now() WHERE user_id = $1 AND read_at IS NULL`, userID)
	return err
}

// ClaimDigest records that today's summary email of this kind is going to the
// person. It returns false if it was already claimed, so a summary is sent once
// a day even with several servers running.
func ClaimDigest(ctx context.Context, q db.Querier, userID uuid.UUID, day time.Time, kind string) (bool, error) {
	tag, err := q.Exec(ctx, `INSERT INTO notification_digests (user_id, day, kind) VALUES ($1, $2::date, $3)
		ON CONFLICT DO NOTHING`, userID, day.Format("2006-01-02"), kind)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
