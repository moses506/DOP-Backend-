// Package events is a small in-process event bus so modules can react to each
// other without importing each other's internals. Example: when deliveries
// publishes "delivery.signed", invoicing can mark the order ready to invoice.
//
// Publish only after the transaction that produced the event has committed.
// Handlers run synchronously; a failing handler is logged and does not affect
// the publisher. If a reaction must never be lost, move to a transactional
// outbox table (see README "Next steps").
package events

import (
	"context"
	"log/slog"
	"sync"
)

type Event struct {
	Name    string // "<module>.<past-tense-verb>", e.g. "client.registered"
	Payload any
}

type Handler func(ctx context.Context, e Event) error

type Bus struct {
	mu   sync.RWMutex
	subs map[string][]Handler
	log  *slog.Logger
}

func NewBus(log *slog.Logger) *Bus {
	return &Bus{subs: map[string][]Handler{}, log: log}
}

func (b *Bus) Subscribe(name string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[name] = append(b.subs[name], h)
}

func (b *Bus) Publish(ctx context.Context, e Event) {
	b.mu.RLock()
	hs := append([]Handler(nil), b.subs[e.Name]...)
	b.mu.RUnlock()
	for _, h := range hs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					b.log.Error("event handler panic", "event", e.Name, "panic", r)
				}
			}()
			if err := h(ctx, e); err != nil {
				b.log.Error("event handler failed", "event", e.Name, "err", err)
			}
		}()
	}
}
