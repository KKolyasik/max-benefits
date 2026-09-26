package session

import (
	"context"
	"encoding/json"
	"sync"
)

// Memory keeps sessions in process memory. Progress is lost on restart, so
// use it for tests and local runs only.
type Memory struct {
	mu   sync.Mutex
	data map[int64][]byte
}

var _ Store = (*Memory)(nil)

func NewMemory() *Memory {
	return &Memory{data: map[int64][]byte{}}
}

// Sessions are stored serialized, so callers never share mutable state with
// the store, exactly like with Redis.

func (m *Memory) Load(_ context.Context, userID int64) (*Session, error) {
	m.mu.Lock()
	raw, ok := m.data[userID]
	m.mu.Unlock()
	if !ok {
		return New(userID), nil
	}
	return decode(raw)
}

func (m *Memory) Save(_ context.Context, s *Session) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.data[s.UserID] = raw
	m.mu.Unlock()
	return nil
}

func (m *Memory) Delete(_ context.Context, userID int64) error {
	m.mu.Lock()
	delete(m.data, userID)
	m.mu.Unlock()
	return nil
}

func decode(raw []byte) (*Session, error) {
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if s.Answers == nil {
		s.Answers = map[string][]string{}
	}
	if s.State == "" {
		s.State = StateMenu
	}
	return &s, nil
}
