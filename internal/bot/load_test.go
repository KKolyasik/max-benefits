package bot

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/KKolyasik/max-benefits/internal/dispatch"
	"github.com/KKolyasik/max-benefits/internal/session"
)

// slowMessenger imitates the network: every call to MAX takes some time.
type slowMessenger struct {
	delay time.Duration
	mu    sync.Mutex
	last  map[int64]Message
}

func (m *slowMessenger) Send(_ context.Context, userID int64, msg Message) error {
	time.Sleep(m.delay)
	m.mu.Lock()
	m.last[userID] = msg
	m.mu.Unlock()
	return nil
}

func (m *slowMessenger) AnswerCallback(context.Context, int64, string, CallbackAnswer) error {
	time.Sleep(m.delay)
	return nil
}

// TestPeakLoad simulates the peak from the requirements: many students fill
// in the questionnaire at the same moment. Each one opens the bot, picks
// "Льготы", answers every question and gets the results.
func TestPeakLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	const users = 300
	sv, kb := loadData(t)
	out := &slowMessenger{delay: 20 * time.Millisecond, last: map[int64]Message{}}
	b := New(sv, kb, session.NewMemory(), out, slog.New(slog.DiscardHandler))

	var (
		mu      sync.Mutex
		slowest time.Duration
	)
	pool := dispatch.New(32, 64, func(ev Event) int64 { return ev.UserID }, func(ev Event) {
		start := time.Now()
		if err := b.Handle(context.Background(), ev); err != nil {
			t.Error(err)
		}
		mu.Lock()
		slowest = max(slowest, time.Since(start))
		mu.Unlock()
	})

	c, _ := sv.Category("benefits")
	script := []string{payload(actCategory, c.ID)}
	for _, qid := range c.Questions {
		q, _ := sv.Question(qid)
		if q.Multi {
			script = append(script, payload(actToggle, q.ID, q.Options[0].ID), payload(actDone, q.ID))
		} else {
			script = append(script, payload(actAnswer, q.ID, q.Options[0].ID))
		}
	}

	start := time.Now()
	for id := int64(1); id <= users; id++ {
		pool.Submit(context.Background(), Event{Type: EventStart, UserID: id})
	}
	for _, p := range script {
		for id := int64(1); id <= users; id++ {
			pool.Submit(context.Background(), Event{Type: EventCallback, UserID: id, CallbackID: "cb", Payload: p})
		}
	}
	pool.Close()
	total := time.Since(start)

	for id := int64(1); id <= users; id++ {
		if msg := out.last[id]; msg.Text == "" || msg.Keyboard == nil || msg.Keyboard[0][0].Payload != actMenu {
			t.Fatalf("user %d did not get the results, last message: %q", id, msg.Text)
		}
	}
	t.Logf("%d users × %d steps in %v, slowest step %v", users, len(script)+1, total, slowest)
	if slowest > 10*time.Second {
		t.Fatalf("a step took %v, the requirement is under 10s", slowest)
	}
}
