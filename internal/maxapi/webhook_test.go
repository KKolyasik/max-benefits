package maxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/KKolyasik/max-benefits/internal/bot"
)

const testSecret = "s3cret-for-tests"

// postUpdate sends one update the way MAX sends it to a webhook.
func postUpdate(h http.Handler, secret string, update []byte) int {
	r := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(update))
	if secret != "" {
		r.Header.Set("X-Max-Bot-Api-Secret", secret)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// testUpdates splits updatesJSON into the single updates a webhook gets.
func testUpdates(t *testing.T) []json.RawMessage {
	t.Helper()
	var list struct {
		Updates []json.RawMessage `json:"updates"`
	}
	if err := json.Unmarshal([]byte(updatesJSON), &list); err != nil {
		t.Fatal(err)
	}
	return list.Updates
}

func TestWebhookConvertsUpdates(t *testing.T) {
	c := newTestClient(t, &fakeMAX{t: t})
	var events []bot.Event
	h := c.Webhook(testSecret, func(ev bot.Event) bool {
		events = append(events, ev)
		return true
	})
	// The group chat message is acknowledged too, so MAX does not resend it.
	for _, u := range testUpdates(t) {
		if code := postUpdate(h, testSecret, u); code != http.StatusOK {
			t.Fatalf("status %d for %s", code, u)
		}
	}
	if !slices.Equal(events, wantEvents) {
		t.Fatalf("got events %+v, want %+v", events, wantEvents)
	}
}

func TestWebhookRejectsRequestsWithoutSecret(t *testing.T) {
	c := newTestClient(t, &fakeMAX{t: t})
	h := c.Webhook(testSecret, func(ev bot.Event) bool {
		t.Errorf("event from a request without the secret: %+v", ev)
		return true
	})
	start := testUpdates(t)[0]
	for _, s := range []string{"", "wrong"} {
		if code := postUpdate(h, s, start); code != http.StatusUnauthorized {
			t.Errorf("secret %q: status %d, want 401", s, code)
		}
	}
}

// When the bot is stopping it cannot take new events: MAX must get an error
// and deliver the update again after the restart.
func TestWebhookAsksToRedeliverUntakenEvent(t *testing.T) {
	c := newTestClient(t, &fakeMAX{t: t})
	h := c.Webhook(testSecret, func(bot.Event) bool { return false })
	if code := postUpdate(h, testSecret, testUpdates(t)[0]); code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
}

func TestSubscribeReplacesOtherWebhooks(t *testing.T) {
	const (
		ours = "https://bot.example.ru/webhook"
		old  = "https://old.example.ru/hook"
	)
	fake := &fakeMAX{t: t, webhooks: []string{old, ours}}
	c := newTestClient(t, fake)
	ctx := context.Background()
	if err := c.Subscribe(ctx, ours, testSecret); err != nil {
		t.Fatal(err)
	}

	hooks, err := c.Webhooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hooks, []string{ours}) {
		t.Fatalf("webhooks after subscribe: %v", hooks)
	}
	subs := fake.sent("/subscriptions")
	if len(subs) != 1 || subs[0].body["url"] != ours || subs[0].body["secret"] != testSecret {
		t.Fatalf("expected one subscription with the secret, got %+v", subs)
	}
	// Re-subscribing on start must not remove the bot's own webhook.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, r := range fake.requests {
		if r.method == http.MethodDelete && r.query["url"] != old {
			t.Errorf("unexpected unsubscribe: %v", r.query)
		}
	}
}
