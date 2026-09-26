// Package session stores per-user dialogue state and questionnaire answers.
package session

import (
	"context"
	"time"
)

// State is the dialogue state of the bot's finite state machine.
type State string

const (
	// StateMenu: the user is in the main menu, nothing is in progress.
	StateMenu State = "menu"
	// StateConfirm: the category's questionnaire is already filled, the bot
	// asked whether to reuse it or fill it again.
	StateConfirm State = "confirm"
	// StateSurvey: the user is answering questions of Category.
	StateSurvey State = "survey"
)

// Session is everything the bot remembers about a user. Answers are saved
// after every step, so questionnaire progress survives restarts.
type Session struct {
	UserID   int64  `json:"user_id"`
	State    State  `json:"state"`
	Category string `json:"category,omitempty"`
	// Selected holds options ticked in the current multi-choice question
	// that are not confirmed with "Готово" yet.
	Selected []string `json:"selected,omitempty"`
	// Answers maps question IDs to chosen option IDs. Questions are shared
	// between categories, so the user is never asked the same thing twice.
	Answers   map[string][]string `json:"answers,omitempty"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// New returns an empty session for a user who has never talked to the bot.
func New(userID int64) *Session {
	return &Session{UserID: userID, State: StateMenu, Answers: map[string][]string{}}
}

// Store persists sessions.
type Store interface {
	// Load returns the user's session or a fresh one if there is none.
	Load(ctx context.Context, userID int64) (*Session, error)
	Save(ctx context.Context, s *Session) error
	// Delete forgets everything about the user.
	Delete(ctx context.Context, userID int64) error
}
