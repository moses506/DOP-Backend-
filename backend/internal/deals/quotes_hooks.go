package deals

import (
	"context"

	"github.com/google/uuid"

	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
)

// Hooks for the quotations module. They run inside the quotation's own
// transaction (q), so the deal and the quotation change together or not at
// all. The quotations module has already checked who may do this.

// QuoteSent records that a quotation went to the client: an early-stage deal
// moves to Proposal, and the deal's value becomes the quotation's total.
func (s *Service) QuoteSent(ctx context.Context, q db.Querier, dealID uuid.UUID, quoteRef, total, currency string) error {
	d, err := getDeal(ctx, q, dealID, true)
	if db.IsNoRows(err) {
		return httpx.ErrNotFound
	}
	if err != nil || d.Stage.Closed() {
		return err
	}
	before := map[string]any{"stage": string(d.Stage), "value": deref(d.Value), "currency": d.Currency}
	moved := d.Stage == StageLead || d.Stage == StageQualified
	if moved {
		d.Stage, d.Probability, d.StageChangedAt = StageProposal, StageProposal.DefaultProbability(), s.now()
	}
	d.Value, d.Currency = &total, currency
	if d.NextStep == nil {
		next := "Follow up on quotation " + quoteRef
		d.NextStep = &next
	}
	if err := updateDeal(ctx, q, d); err != nil {
		return err
	}
	summary := "Quotation " + quoteRef + " sent — value " + currency + " " + total
	if moved {
		summary += "; moved to Proposal"
	}
	return audit.Record(ctx, q, audit.Entry{EntityType: entityType, EntityID: d.ID, Action: "quotation_sent", Summary: summary,
		Changes: audit.Diff(before, map[string]any{"stage": string(d.Stage), "value": total, "currency": currency})})
}

// QuoteAccepted marks the deal won at the accepted quotation's value.
func (s *Service) QuoteAccepted(ctx context.Context, q db.Querier, dealID uuid.UUID, quoteRef, total, currency string) error {
	d, err := getDeal(ctx, q, dealID, true)
	if db.IsNoRows(err) {
		return httpx.ErrNotFound
	}
	if err != nil {
		return err
	}
	if d.Stage == StageWon {
		return nil
	}
	from := d.Stage
	now := s.now()
	d.Stage, d.Probability, d.StageChangedAt, d.ClosedAt, d.LostReason = StageWon, 100, now, &now, nil
	d.Value, d.Currency = &total, currency
	if err := updateDeal(ctx, q, d); err != nil {
		return err
	}
	if err := audit.Record(ctx, q, audit.Entry{EntityType: entityType, EntityID: d.ID, Action: "won",
		Summary: "Deal won 🎉 — quotation " + quoteRef + " accepted",
		Changes: map[string]any{"stage": map[string]any{"from": from, "to": StageWon}, "value": total}}); err != nil {
		return err
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
