package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KKolyasik/max-benefits/internal/feedback"
)

const feedbackColumns = "id, user_id, category, question, answers, text, created_at"

// AddFeedback stores what a student wrote and returns its ID.
func (s *Store) AddFeedback(ctx context.Context, f feedback.Feedback) (int64, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	answers := f.Answers
	if answers == nil {
		answers = map[string][]string{}
	}
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO feedback (user_id, category, question, answers, text, created_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		f.UserID, f.Category, f.Question, answers, f.Text, f.CreatedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store feedback: %w", err)
	}
	return id, nil
}

// UserFeedbackSince counts what the user has written since the time.
func (s *Store) UserFeedbackSince(ctx context.Context, userID int64, since time.Time) (int, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var n int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM feedback WHERE user_id = $1 AND created_at > $2",
		userID, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("count feedback of a user: %w", err)
	}
	return n, nil
}

// PendingFeedback counts the feedback no admin has resolved yet.
func (s *Store) PendingFeedback(ctx context.Context) (int, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var n int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM feedback WHERE resolved_at IS NULL").Scan(&n); err != nil {
		return 0, fmt.Errorf("count feedback: %w", err)
	}
	return n, nil
}

// NextFeedback returns the oldest unresolved feedback with an ID above
// after; zero starts from the beginning.
func (s *Store) NextFeedback(ctx context.Context, after int64) (feedback.Feedback, bool, error) {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var f feedback.Feedback
	err := s.db.QueryRow(ctx, "SELECT "+feedbackColumns+
		" FROM feedback WHERE resolved_at IS NULL AND id > $1 ORDER BY id LIMIT 1", after).
		Scan(&f.ID, &f.UserID, &f.Category, &f.Question, &f.Answers, &f.Text, &f.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return feedback.Feedback{}, false, nil
	}
	if err != nil {
		return feedback.Feedback{}, false, fmt.Errorf("read feedback: %w", err)
	}
	return f, true, nil
}

// ForgetFeedbackAnswers deletes the answers kept with the user's feedback:
// the user deleted their data. The texts stay.
func (s *Store) ForgetFeedbackAnswers(ctx context.Context, userID int64) error {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	if _, err := s.db.Exec(ctx, "UPDATE feedback SET answers = '{}' WHERE user_id = $1 AND answers <> '{}'", userID); err != nil {
		return fmt.Errorf("forget the answers of user %d: %w", userID, err)
	}
	return nil
}

// ResolveFeedback marks the feedback resolved by the admin. Feedback resolved
// before keeps the admin who did it first.
func (s *Store) ResolveFeedback(ctx context.Context, id, admin int64) error {
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	if _, err := s.db.Exec(ctx, "UPDATE feedback SET resolved_at = now(), resolved_by = $2 WHERE id = $1 AND resolved_at IS NULL",
		id, admin); err != nil {
		return fmt.Errorf("resolve feedback %d: %w", id, err)
	}
	return nil
}
