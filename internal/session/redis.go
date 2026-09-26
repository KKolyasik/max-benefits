package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis keeps sessions as JSON values, one key per user.
type Redis struct {
	client *redis.Client
	prefix string
	// ttl expires sessions of users who stopped using the bot; 0 keeps them
	// forever. Every save extends it.
	ttl time.Duration
}

var _ Store = (*Redis)(nil)

func NewRedis(client *redis.Client, ttl time.Duration) *Redis {
	return &Redis{client: client, prefix: "session:", ttl: ttl}
}

func (r *Redis) key(userID int64) string {
	return r.prefix + strconv.FormatInt(userID, 10)
}

func (r *Redis) Load(ctx context.Context, userID int64) (*Session, error) {
	raw, err := r.client.Get(ctx, r.key(userID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return New(userID), nil
	}
	if err != nil {
		return nil, fmt.Errorf("redis get: %w", err)
	}
	s, err := decode(raw)
	if err != nil {
		return nil, fmt.Errorf("decode session %d: %w", userID, err)
	}
	return s, nil
}

func (r *Redis) Save(ctx context.Context, s *Session) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := r.client.Set(ctx, r.key(s.UserID), raw, r.ttl).Err(); err != nil {
		return fmt.Errorf("redis set: %w", err)
	}
	return nil
}

func (r *Redis) Delete(ctx context.Context, userID int64) error {
	if err := r.client.Del(ctx, r.key(userID)).Err(); err != nil {
		return fmt.Errorf("redis del: %w", err)
	}
	return nil
}
