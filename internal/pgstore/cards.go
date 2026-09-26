package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
)

var _ knowledge.Base = (*Store)(nil)

// Find implements knowledge.Base: it reads the category's cards and picks
// them the same way the YAML base does.
func (s *Store) Find(ctx context.Context, req knowledge.Request) ([]knowledge.Entry, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	cards, err := s.query(ctx, `SELECT card FROM cards WHERE card -> 'categories' ? $1 ORDER BY position`, req.Category)
	if err != nil {
		return nil, err
	}
	cards = slices.DeleteFunc(cards, func(c knowledge.Card) bool {
		return len(knowledge.ValidateCard(c, s.survey)) > 0
	})
	return knowledge.Pick(cards, req), nil
}

// Cards returns every card in the order they were added.
func (s *Store) Cards(ctx context.Context) ([]knowledge.Card, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	return s.query(ctx, "SELECT card FROM cards ORDER BY position")
}

// Card returns the card with the ID.
func (s *Store) Card(ctx context.Context, id string) (knowledge.Card, bool, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var c knowledge.Card
	err := s.db.QueryRow(ctx, "SELECT card FROM cards WHERE id = $1", id).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.Card{}, false, nil
	}
	if err != nil {
		return knowledge.Card{}, false, fmt.Errorf("read card %s: %w", id, err)
	}
	return c, true, nil
}

// Broken returns the cards that don't match the survey any more, with the
// reasons. Such cards are not shown; the bot logs them at startup.
func (s *Store) Broken(ctx context.Context) (map[string][]error, error) {
	cards, err := s.Cards(ctx)
	if err != nil {
		return nil, err
	}
	broken := map[string][]error{}
	for _, c := range cards {
		if errs := knowledge.ValidateCard(c, s.survey); len(errs) > 0 {
			broken[c.ID] = errs
		}
	}
	return broken, nil
}

// Seed imports the cards if the base is empty, which happens on the first
// start with a database. It returns how many cards it imported.
func (s *Store) Seed(ctx context.Context, cards []knowledge.Card, note string) (int, error) {
	imported := 0
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM cards)").Scan(&exists); err != nil || exists {
			return err
		}
		for _, c := range cards {
			data, err := json.Marshal(c)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO cards (id, card) VALUES ($1, $2)", c.ID, data); err != nil {
				return fmt.Errorf("import card %s: %w", c.ID, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO card_history (card_id, card, note) VALUES ($1, $2, $3)",
				c.ID, data, note); err != nil {
				return fmt.Errorf("import card %s: %w", c.ID, err)
			}
		}
		imported = len(cards)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("import cards: %w", err)
	}
	return imported, nil
}

func (s *Store) query(ctx context.Context, sql string, args ...any) ([]knowledge.Card, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read cards: %w", err)
	}
	cards, err := pgx.CollectRows(rows, pgx.RowTo[knowledge.Card])
	if err != nil {
		return nil, fmt.Errorf("read cards: %w", err)
	}
	return cards, nil
}
