// Package pgstore keeps the knowledge base and the agent's drafts in
// PostgreSQL. Selections are read from the database on every request, so a
// card an admin approves is shown to students at once, by every bot
// instance, with nothing to reload.
package pgstore

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KKolyasik/max-benefits/internal/survey"
)

//go:embed migrations/*.sql
var migrations embed.FS

// lockKey is an arbitrary advisory lock key. It keeps bot instances that
// start at the same time from migrating or importing twice.
const lockKey = 7_246_101

// Store implements knowledge.Base and the moderation of drafts.
type Store struct {
	db     *pgxpool.Pool
	survey *survey.Survey
	// timeout bounds each query: a student waiting for a selection should
	// get "try later" rather than a hanging chat.
	timeout time.Duration
}

// Connect opens a connection pool and checks the database is reachable.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.Ping(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	return db, nil
}

// New returns a store over the pool. Cards are checked against the survey:
// one that no longer matches it (say, an option was removed) is not shown.
func New(db *pgxpool.Pool, s *survey.Survey) *Store {
	return &Store{db: db, survey: s, timeout: 5 * time.Second}
}

// Migrate applies the migrations the database doesn't have yet.
func Migrate(ctx context.Context, db *pgxpool.Pool) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)

	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			name text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		rows, err := tx.Query(ctx, "SELECT name FROM schema_migrations")
		if err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		applied, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		for _, name := range names {
			if slices.Contains(applied, name) {
				continue
			}
			sql, err := migrations.ReadFile(name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", name); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
		}
		return nil
	})
}

func (s *Store) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.timeout)
}
