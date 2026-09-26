package maxapi

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// limiter paces outgoing requests: globally and per dialog.
type limiter struct {
	global *rate.Limiter
	pause  time.Duration

	mu        sync.Mutex
	dialogs   map[int64]*dialogLimit
	lastSweep time.Time
}

type dialogLimit struct {
	lim  *rate.Limiter
	used time.Time
}

func newLimiter(globalRPS int, dialogPause time.Duration) *limiter {
	return &limiter{
		global:    rate.NewLimiter(rate.Limit(globalRPS), globalRPS),
		pause:     dialogPause,
		dialogs:   map[int64]*dialogLimit{},
		lastSweep: time.Now(),
	}
}

// wait blocks until a request to the user's dialog is allowed.
func (l *limiter) wait(ctx context.Context, userID int64) error {
	if err := l.dialog(userID).Wait(ctx); err != nil {
		return err
	}
	return l.global.Wait(ctx)
}

func (l *limiter) dialog(userID int64) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// A limiter idle for a while is full again, so forgetting it is safe and
	// keeps memory bounded by the number of recently active users.
	if now.Sub(l.lastSweep) > time.Minute {
		for id, d := range l.dialogs {
			if now.Sub(d.used) > time.Minute {
				delete(l.dialogs, id)
			}
		}
		l.lastSweep = now
	}
	d, ok := l.dialogs[userID]
	if !ok {
		d = &dialogLimit{lim: rate.NewLimiter(rate.Every(l.pause), 1)}
		l.dialogs[userID] = d
	}
	d.used = now
	return d.lim
}
