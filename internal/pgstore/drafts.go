package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/moderation"
)

const draftColumns = "id, card, updates, query, sources, notes, found_at, status"

// AddDraft stores a draft from the agent. externalID identifies it at the
// source: a draft delivered twice is stored once. It reports whether the
// draft is new.
func (s *Store) AddDraft(ctx context.Context, externalID string, d moderation.Draft) (bool, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	card, err := json.Marshal(d.Card)
	if err != nil {
		return false, err
	}
	tag, err := s.db.Exec(ctx, `INSERT INTO drafts (external_id, card, updates, query, sources, notes, found_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (external_id) DO NOTHING`,
		externalID, card, d.Updates, d.Query, nonNil(d.Sources), nonNil(d.Notes), d.FoundAt)
	if err != nil {
		return false, fmt.Errorf("store draft: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// PendingDrafts counts drafts waiting for review.
func (s *Store) PendingDrafts(ctx context.Context) (int, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var n int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM drafts WHERE status = 'pending'").Scan(&n); err != nil {
		return 0, fmt.Errorf("count drafts: %w", err)
	}
	return n, nil
}

// NextDraft returns the oldest pending draft with an ID above after; zero
// starts from the beginning.
func (s *Store) NextDraft(ctx context.Context, after int64) (moderation.Draft, bool, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	row := s.db.QueryRow(ctx, "SELECT "+draftColumns+
		" FROM drafts WHERE status = 'pending' AND id > $1 ORDER BY id LIMIT 1", after)
	d, err := scanDraft(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return moderation.Draft{}, false, nil
	}
	if err != nil {
		return moderation.Draft{}, false, fmt.Errorf("read draft: %w", err)
	}
	return d, true, nil
}

// ApproveDraft publishes the draft's card: it is shown to students from the
// next request on. It returns the card and whether it replaced an existing
// one. The checks run in the same transaction as the write, so the draft is
// approved once and only if it still fits the base and the survey.
func (s *Store) ApproveDraft(ctx context.Context, id, admin int64) (knowledge.Card, bool, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var card knowledge.Card
	var replaced bool
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		d, err := lockDraft(ctx, tx, id)
		if err != nil {
			return err
		}
		var current *knowledge.Card
		var c knowledge.Card
		switch err := tx.QueryRow(ctx, "SELECT card FROM cards WHERE id = $1 FOR UPDATE", d.Card.ID).Scan(&c); {
		case err == nil:
			current = &c
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if problems := moderation.Problems(d, s.survey, current); len(problems) > 0 {
			return &moderation.InvalidError{Problems: problems}
		}

		data, err := json.Marshal(d.Card)
		if err != nil {
			return err
		}
		if current != nil {
			_, err = tx.Exec(ctx, "UPDATE cards SET card = $2, updated_at = now(), updated_by = $3 WHERE id = $1",
				d.Card.ID, data, admin)
		} else {
			_, err = tx.Exec(ctx, "INSERT INTO cards (id, card, updated_by) VALUES ($1, $2, $3)", d.Card.ID, data, admin)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO card_history (card_id, card, changed_by, draft_id) VALUES ($1, $2, $3, $4)",
			d.Card.ID, data, admin, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE drafts SET status = 'approved', decided_at = now(), decided_by = $2 WHERE id = $1",
			id, admin); err != nil {
			return err
		}
		card, replaced = d.Card, current != nil
		return nil
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// Another draft with the same new card was approved a moment ago.
		return knowledge.Card{}, false, &moderation.InvalidError{Problems: []string{"a card with this id was just added"}}
	}
	if err != nil {
		return knowledge.Card{}, false, fmt.Errorf("approve draft %d: %w", id, err)
	}
	return card, replaced, nil
}

// RejectDraft marks the draft as rejected.
func (s *Store) RejectDraft(ctx context.Context, id, admin int64) error {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := lockDraft(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE drafts SET status = 'rejected', decided_at = now(), decided_by = $2 WHERE id = $1",
			id, admin)
		return err
	})
	if err != nil {
		return fmt.Errorf("reject draft %d: %w", id, err)
	}
	return nil
}

// lockDraft reads a pending draft and locks it until the transaction ends.
func lockDraft(ctx context.Context, tx pgx.Tx, id int64) (moderation.Draft, error) {
	d, err := scanDraft(tx.QueryRow(ctx, "SELECT "+draftColumns+" FROM drafts WHERE id = $1 FOR UPDATE", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return moderation.Draft{}, moderation.ErrNotFound
	}
	if err != nil {
		return moderation.Draft{}, err
	}
	if d.Status != moderation.Pending {
		return moderation.Draft{}, moderation.ErrDecided
	}
	return d, nil
}

func scanDraft(row pgx.Row) (moderation.Draft, error) {
	var d moderation.Draft
	var status string
	err := row.Scan(&d.ID, &d.Card, &d.Updates, &d.Query, &d.Sources, &d.Notes, &d.FoundAt, &status)
	d.Status = moderation.Status(status)
	return d, err
}

func nonNil(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}
