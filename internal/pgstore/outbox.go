package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KKolyasik/max-benefits/internal/moderation"
)

// Kinds of outbox events.
const (
	// EventCard means the card Ref changed.
	EventCard = "card"
	// EventDecision means admins decided on the draft Ref.
	EventDecision = "decision"
)

// Event is a change the agent must learn about through Kafka. It names what
// changed rather than holding it, so the relay publishes the state at the
// moment of publishing: a card changed twice goes out in its last version.
type Event struct {
	ID   int64
	Kind string
	Ref  string
}

// Events returns up to limit of the oldest events waiting in the outbox.
func (s *Store) Events(ctx context.Context, limit int) ([]Event, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	rows, err := s.db.Query(ctx, "SELECT id, kind, ref FROM outbox ORDER BY id LIMIT $1", limit)
	if err != nil {
		return nil, fmt.Errorf("read outbox: %w", err)
	}
	events, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Event])
	if err != nil {
		return nil, fmt.Errorf("read outbox: %w", err)
	}
	return events, nil
}

// DeleteEvents removes published events from the outbox.
func (s *Store) DeleteEvents(ctx context.Context, ids []int64) error {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	if _, err := s.db.Exec(ctx, "DELETE FROM outbox WHERE id = ANY($1)", ids); err != nil {
		return fmt.Errorf("clean outbox: %w", err)
	}
	return nil
}

// Decision is what admins decided about a draft.
type Decision struct {
	// ExternalID is the ID of the draft at the source.
	ExternalID string
	// CardID is the card the draft is about.
	CardID    string
	Status    moderation.Status
	DecidedAt time.Time
}

// DraftDecision returns the decision on the draft ref, an EventDecision
// reference; false if the draft is not there or not decided by admins.
func (s *Store) DraftDecision(ctx context.Context, ref string) (Decision, bool, error) {
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return Decision{}, false, fmt.Errorf("draft reference %q: %w", ref, err)
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var d Decision
	var status string
	var decidedAt *time.Time
	err = s.db.QueryRow(ctx, "SELECT external_id, "+draftTarget+", status, decided_at FROM drafts WHERE id = $1", id).
		Scan(&d.ExternalID, &d.CardID, &status, &decidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, false, nil
	}
	if err != nil {
		return Decision{}, false, fmt.Errorf("read decision on draft %d: %w", id, err)
	}
	d.Status = moderation.Status(status)
	if decidedAt == nil || (d.Status != moderation.Approved && d.Status != moderation.Rejected) {
		return Decision{}, false, nil
	}
	d.DecidedAt = *decidedAt
	return d, true, nil
}
